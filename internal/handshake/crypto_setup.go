package handshake

import (
	"crypto/aes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"unsafe"

	"github.com/DrakenLibra/gt-bbr/internal/protocol"
	"github.com/DrakenLibra/gt-bbr/internal/qerr"
	"github.com/DrakenLibra/gt-bbr/internal/utils"
	"github.com/marten-seemann/qtls"
)

// TLS unexpected_message alert
const alertUnexpectedMessage uint8 = 10

type messageType uint8

// TLS handshake message types.
const (
	typeClientHello         messageType = 1
	typeServerHello         messageType = 2
	typeNewSessionTicket    messageType = 4
	typeEncryptedExtensions messageType = 8
	typeCertificate         messageType = 11
	typeCertificateRequest  messageType = 13
	typeCertificateVerify   messageType = 15
	typeFinished            messageType = 20
)

func (m messageType) String() string {
	switch m {
	case typeClientHello:
		return "ClientHello"
	case typeServerHello:
		return "ServerHello"
	case typeNewSessionTicket:
		return "NewSessionTicket"
	case typeEncryptedExtensions:
		return "EncryptedExtensions"
	case typeCertificate:
		return "Certificate"
	case typeCertificateRequest:
		return "CertificateRequest"
	case typeCertificateVerify:
		return "CertificateVerify"
	case typeFinished:
		return "Finished"
	default:
		return fmt.Sprintf("unknown message type: %d", m)
	}
}

var (
	// ErrOpenerNotYetAvailable is returned when an opener is requested for an encryption level,
	// but the corresponding opener has not yet been initialized
	// This can happen when packets arrive out of order.
	ErrOpenerNotYetAvailable = errors.New("CryptoSetup: opener at this encryption level not yet available")
	// ErrKeysDropped is returned when an opener or a sealer is requested for an encryption level,
	// but the corresponding keys have already been dropped.
	ErrKeysDropped = errors.New("CryptoSetup: keys were already dropped")
)

type cryptoSetup struct {
	tlsConf *qtls.Config
	conn    *qtls.Conn

	messageChan chan []byte

	paramsChan <-chan []byte

	runner handshakeRunner

	closed    bool
	alertChan chan uint8
	// handshakeDone is closed as soon as the go routine running qtls.Handshake() returns
	handshakeDone chan struct{}
	// is closed when Close() is called
	closeChan chan struct{}

	clientHelloWritten     bool
	clientHelloWrittenChan chan struct{}

	receivedWriteKey chan struct{}
	receivedReadKey  chan struct{}
	// WriteRecord does a non-blocking send on this channel.
	// This way, handleMessage can see if qtls tries to write a message.
	// This is necessary:
	// for servers: to see if a HelloRetryRequest should be sent in response to a ClientHello
	// for clients: to see if a ServerHello is a HelloRetryRequest
	writeRecord chan struct{}

	logger utils.Logger

	perspective protocol.Perspective

	mutex sync.Mutex // protects all members below

	readEncLevel  protocol.EncryptionLevel
	writeEncLevel protocol.EncryptionLevel

	initialStream io.Writer
	initialOpener Opener
	initialSealer Sealer

	handshakeStream io.Writer
	handshakeOpener Opener
	handshakeSealer Sealer

	oneRTTStream io.Writer
	opener       Opener
	sealer       Sealer
}

var _ qtls.RecordLayer = &cryptoSetup{}
var _ CryptoSetup = &cryptoSetup{}

// NewCryptoSetupClient creates a new crypto setup for the client
func NewCryptoSetupClient(
	initialStream io.Writer,
	handshakeStream io.Writer,
	oneRTTStream io.Writer,
	connID protocol.ConnectionID,
	remoteAddr net.Addr,
	tp *TransportParameters,
	runner handshakeRunner,
	tlsConf *tls.Config,
	logger utils.Logger,
) (CryptoSetup, <-chan struct{} /* ClientHello written */, error) {
	cs, clientHelloWritten, err := newCryptoSetup(
		initialStream,
		handshakeStream,
		oneRTTStream,
		connID,
		tp,
		runner,
		tlsConf,
		logger,
		protocol.PerspectiveClient,
	)
	if err != nil {
		return nil, nil, err
	}
	cs.conn = qtls.Client(newConn(remoteAddr), cs.tlsConf)
	return cs, clientHelloWritten, nil
}

// NewCryptoSetupServer creates a new crypto setup for the server
func NewCryptoSetupServer(
	initialStream io.Writer,
	handshakeStream io.Writer,
	oneRTTStream io.Writer,
	connID protocol.ConnectionID,
	remoteAddr net.Addr,
	tp *TransportParameters,
	runner handshakeRunner,
	tlsConf *tls.Config,
	logger utils.Logger,
) (CryptoSetup, error) {
	cs, _, err := newCryptoSetup(
		initialStream,
		handshakeStream,
		oneRTTStream,
		connID,
		tp,
		runner,
		tlsConf,
		logger,
		protocol.PerspectiveServer,
	)
	if err != nil {
		return nil, err
	}
	cs.conn = qtls.Server(newConn(remoteAddr), cs.tlsConf)
	return cs, nil
}

func newCryptoSetup(
	initialStream io.Writer,
	handshakeStream io.Writer,
	oneRTTStream io.Writer,
	connID protocol.ConnectionID,
	tp *TransportParameters,
	runner handshakeRunner,
	tlsConf *tls.Config,
	logger utils.Logger,
	perspective protocol.Perspective,
) (*cryptoSetup, <-chan struct{} /* ClientHello written */, error) {
	initialSealer, initialOpener, err := NewInitialAEAD(connID, perspective)
	if err != nil {
		return nil, nil, err
	}
	extHandler := newExtensionHandler(tp.Marshal(), perspective)
	cs := &cryptoSetup{
		initialStream:          initialStream,
		initialSealer:          initialSealer,
		initialOpener:          initialOpener,
		handshakeStream:        handshakeStream,
		oneRTTStream:           oneRTTStream,
		readEncLevel:           protocol.EncryptionInitial,
		writeEncLevel:          protocol.EncryptionInitial,
		runner:                 runner,
		paramsChan:             extHandler.TransportParameters(),
		logger:                 logger,
		perspective:            perspective,
		handshakeDone:          make(chan struct{}),
		alertChan:              make(chan uint8),
		clientHelloWrittenChan: make(chan struct{}),
		messageChan:            make(chan []byte, 100),
		receivedReadKey:        make(chan struct{}),
		receivedWriteKey:       make(chan struct{}),
		writeRecord:            make(chan struct{}, 1),
		closeChan:              make(chan struct{}),
	}
	qtlsConf := tlsConfigToQtlsConfig(tlsConf, cs, extHandler)
	cs.tlsConf = qtlsConf
	return cs, cs.clientHelloWrittenChan, nil
}

func (h *cryptoSetup) ChangeConnectionID(id protocol.ConnectionID) error {
	initialSealer, initialOpener, err := NewInitialAEAD(id, h.perspective)
	if err != nil {
		return err
	}
	h.initialSealer = initialSealer
	h.initialOpener = initialOpener
	return nil
}

func (h *cryptoSetup) Received1RTTAck() {
	// drop initial keys
	// TODO: do this earlier
	if h.initialOpener != nil {
		h.initialOpener = nil
		h.initialSealer = nil
		h.runner.DropKeys(protocol.EncryptionInitial)
		h.logger.Debugf("Dropping Initial keys.")
	}
	// drop handshake keys
	if h.handshakeOpener != nil {
		h.handshakeOpener = nil
		h.handshakeSealer = nil
		h.logger.Debugf("Dropping Handshake keys.")
		h.runner.DropKeys(protocol.EncryptionHandshake)
	}
}

func (h *cryptoSetup) RunHandshake() {
	// Handle errors that might occur when HandleData() is called.
	handshakeComplete := make(chan struct{})
	handshakeErrChan := make(chan error, 1)
	go func() {
		defer close(h.handshakeDone)
		if err := h.conn.Handshake(); err != nil {
			handshakeErrChan <- err
			return
		}
		close(handshakeComplete)
	}()

	select {
	case <-handshakeComplete: // return when the handshake is done
		h.runner.OnHandshakeComplete()
	case <-h.closeChan:
		close(h.messageChan)
		// wait until the Handshake() go routine has returned
		<-h.handshakeDone
	case alert := <-h.alertChan:
		handshakeErr := <-handshakeErrChan
		h.onError(alert, handshakeErr.Error())
	}
}

func (h *cryptoSetup) onError(alert uint8, message string) {

	h.runner.OnError(qerr.CryptoError(alert, message))
}

func (h *cryptoSetup) Close() error {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if h.closed {
		return nil
	}
	h.closed = true

	close(h.closeChan)
	// wait until qtls.Handshake() actually returned
	<-h.handshakeDone
	return nil
}

// handleMessage handles a TLS handshake message.
// It is called by the crypto streams when a new message is available.
// It returns if it is done with messages on the same encryption level.
func (h *cryptoSetup) HandleMessage(data []byte, encLevel protocol.EncryptionLevel) bool /* stream finished */ {
	msgType := messageType(data[0])
	h.logger.Debugf("Received %s message (%d bytes, encryption level: %s)", msgType, len(data), encLevel)
	if err := h.checkEncryptionLevel(msgType, encLevel); err != nil {
		h.onError(alertUnexpectedMessage, err.Error())
		return false
	}
	h.messageChan <- data
	if encLevel == protocol.Encryption1RTT {
		h.handlePostHandshakeMessage(data)
	}
	switch h.perspective {
	case protocol.PerspectiveClient:
		return h.handleMessageForClient(msgType)
	case protocol.PerspectiveServer:
		return h.handleMessageForServer(msgType)
	default:
		panic("")
	}
}

func (h *cryptoSetup) checkEncryptionLevel(msgType messageType, encLevel protocol.EncryptionLevel) error {
	var expected protocol.EncryptionLevel
	switch msgType {
	case typeClientHello,
		typeServerHello:
		expected = protocol.EncryptionInitial
	case typeEncryptedExtensions,
		typeCertificate,
		typeCertificateRequest,
		typeCertificateVerify,
		typeFinished:
		expected = protocol.EncryptionHandshake
	case typeNewSessionTicket:
		expected = protocol.Encryption1RTT
	default:
		return fmt.Errorf("unexpected handshake message: %d", msgType)
	}
	if encLevel != expected {
		return fmt.Errorf("expected handshake message %s to have encryption level %s, has %s", msgType, expected, encLevel)
	}
	return nil
}

func (h *cryptoSetup) handleMessageForServer(msgType messageType) bool {
	switch msgType {
	case typeClientHello:
		select {
		case <-h.writeRecord:
			// If qtls sends a HelloRetryRequest, it will only write the record.
			// If it accepts the ClientHello, it will first read the transport parameters.
			h.logger.Debugf("Sending HelloRetryRequest")
			return false
		case data := <-h.paramsChan:
			h.runner.OnReceivedParams(data)
		case <-h.handshakeDone:
			return false
		}
		// get the handshake read key
		select {
		case <-h.receivedReadKey:
		case <-h.handshakeDone:
			return false
		}
		// get the handshake write key
		select {
		case <-h.receivedWriteKey:
		case <-h.handshakeDone:
			return false
		}
		// get the 1-RTT write key
		select {
		case <-h.receivedWriteKey:
		case <-h.handshakeDone:
			return false
		}
		return true
	case typeCertificate, typeCertificateVerify:
		// nothing to do
		return false
	case typeFinished:
		// get the 1-RTT read key
		select {
		case <-h.receivedReadKey:
		case <-h.handshakeDone:
			return false
		}
		return true
	default:
		// unexpected message
		return false
	}
}

func (h *cryptoSetup) handleMessageForClient(msgType messageType) bool {
	switch msgType {
	case typeServerHello:
		// get the handshake write key
		select {
		case <-h.writeRecord:
			// If qtls writes in response to a ServerHello, this means that this ServerHello
			// is a HelloRetryRequest.
			// Otherwise, we'd just wait for the Certificate message.
			h.logger.Debugf("ServerHello is a HelloRetryRequest")
			return false
		case <-h.receivedWriteKey:
		case <-h.handshakeDone:
			return false
		}
		// get the handshake read key
		select {
		case <-h.receivedReadKey:
		case <-h.handshakeDone:
			return false
		}
		return true
	case typeEncryptedExtensions:
		select {
		case data := <-h.paramsChan:
			h.runner.OnReceivedParams(data)
		case <-h.handshakeDone:
			return false
		}
		return false
	case typeCertificateRequest, typeCertificate, typeCertificateVerify:
		// nothing to do
		return false
	case typeFinished:
		// get the 1-RTT read key
		select {
		case <-h.receivedReadKey:
		case <-h.handshakeDone:
			return false
		}
		// get the handshake write key
		select {
		case <-h.receivedWriteKey:
		case <-h.handshakeDone:
			return false
		}
		return true
	default:
		return false
	}
}

func (h *cryptoSetup) handlePostHandshakeMessage(data []byte) {
	// make sure the handshake has already completed
	<-h.handshakeDone

	done := make(chan struct{})
	defer close(done)

	// h.alertChan is an unbuffered channel.
	// If an error occurs during conn.HandlePostHandshakeMessage,
	// it will be sent on this channel.
	// Read it from a go-routine so that HandlePostHandshakeMessage doesn't deadlock.
	alertChan := make(chan uint8, 1)
	go func() {
		select {
		case alert := <-h.alertChan:
			alertChan <- alert
		case <-done:
		}
	}()

	if err := h.conn.HandlePostHandshakeMessage(); err != nil {
		h.onError(<-alertChan, err.Error())
	}
}

// ReadHandshakeMessage is called by TLS.
// It blocks until a new handshake message is available.
func (h *cryptoSetup) ReadHandshakeMessage() ([]byte, error) {
	msg, ok := <-h.messageChan
	if !ok {
		return nil, errors.New("error while handling the handshake message")
	}
	return msg, nil
}

func (h *cryptoSetup) SetReadKey(suite *qtls.CipherSuite, trafficSecret []byte) {
	key := qtls.HkdfExpandLabel(suite.Hash(), trafficSecret, []byte{}, "quic key", suite.KeyLen())
	iv := qtls.HkdfExpandLabel(suite.Hash(), trafficSecret, []byte{}, "quic iv", suite.IVLen())
	hpKey := qtls.HkdfExpandLabel(suite.Hash(), trafficSecret, []byte{}, "quic hp", suite.KeyLen())
	hpDecrypter, err := aes.NewCipher(hpKey)
	if err != nil {
		panic(fmt.Sprintf("error creating new AES cipher: %s", err))
	}

	h.mutex.Lock()
	switch h.readEncLevel {
	case protocol.EncryptionInitial:
		h.readEncLevel = protocol.EncryptionHandshake
		h.handshakeOpener = newOpener(suite.AEAD(key, iv), hpDecrypter, false)
		h.logger.Debugf("Installed Handshake Read keys")
	case protocol.EncryptionHandshake:
		h.readEncLevel = protocol.Encryption1RTT
		h.opener = newOpener(suite.AEAD(key, iv), hpDecrypter, true)
		h.logger.Debugf("Installed 1-RTT Read keys")
	default:
		panic("unexpected read encryption level")
	}
	h.mutex.Unlock()
	h.receivedReadKey <- struct{}{}
}

func (h *cryptoSetup) SetWriteKey(suite *qtls.CipherSuite, trafficSecret []byte) {
	key := qtls.HkdfExpandLabel(suite.Hash(), trafficSecret, []byte{}, "quic key", suite.KeyLen())
	iv := qtls.HkdfExpandLabel(suite.Hash(), trafficSecret, []byte{}, "quic iv", suite.IVLen())
	hpKey := qtls.HkdfExpandLabel(suite.Hash(), trafficSecret, []byte{}, "quic hp", suite.KeyLen())
	hpEncrypter, err := aes.NewCipher(hpKey)
	if err != nil {
		panic(fmt.Sprintf("error creating new AES cipher: %s", err))
	}

	h.mutex.Lock()
	switch h.writeEncLevel {
	case protocol.EncryptionInitial:
		h.writeEncLevel = protocol.EncryptionHandshake
		h.handshakeSealer = newSealer(suite.AEAD(key, iv), hpEncrypter, false)
		h.logger.Debugf("Installed Handshake Write keys")
	case protocol.EncryptionHandshake:
		h.writeEncLevel = protocol.Encryption1RTT
		h.sealer = newSealer(suite.AEAD(key, iv), hpEncrypter, true)
		h.logger.Debugf("Installed 1-RTT Write keys")
	default:
		panic("unexpected write encryption level")
	}
	h.mutex.Unlock()
	h.receivedWriteKey <- struct{}{}
}

// WriteRecord is called when TLS writes data
func (h *cryptoSetup) WriteRecord(p []byte) (int, error) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	switch h.writeEncLevel {
	case protocol.EncryptionInitial:
		// assume that the first WriteRecord call contains the ClientHello
		n, err := h.initialStream.Write(p)
		if !h.clientHelloWritten && h.perspective == protocol.PerspectiveClient {
			h.clientHelloWritten = true
			close(h.clientHelloWrittenChan)
		} else {
			// We need additional signaling to properly detect HelloRetryRequests.
			// For servers: when the ServerHello is written.
			// For clients: when a reply is sent in response to a ServerHello.
			h.writeRecord <- struct{}{}
		}
		return n, err
	case protocol.EncryptionHandshake:
		return h.handshakeStream.Write(p)
	case protocol.Encryption1RTT:
		return h.oneRTTStream.Write(p)
	default:
		panic(fmt.Sprintf("unexpected write encryption level: %s", h.writeEncLevel))
	}
}

func (h *cryptoSetup) SendAlert(alert uint8) {
	h.alertChan <- alert
}

func (h *cryptoSetup) GetSealer() (protocol.EncryptionLevel, Sealer) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if h.sealer != nil {
		return protocol.Encryption1RTT, h.sealer
	}
	if h.handshakeSealer != nil {
		return protocol.EncryptionHandshake, h.handshakeSealer
	}
	return protocol.EncryptionInitial, h.initialSealer
}

func (h *cryptoSetup) GetSealerWithEncryptionLevel(level protocol.EncryptionLevel) (Sealer, error) {
	errNoSealer := fmt.Errorf("CryptoSetup: no sealer with encryption level %s", level.String())

	h.mutex.Lock()
	defer h.mutex.Unlock()

	switch level {
	case protocol.EncryptionInitial:
		return h.initialSealer, nil
	case protocol.EncryptionHandshake:
		if h.handshakeSealer == nil {
			return nil, errNoSealer
		}
		return h.handshakeSealer, nil
	case protocol.Encryption1RTT:
		if h.sealer == nil {
			return nil, errNoSealer
		}
		return h.sealer, nil
	default:
		return nil, errNoSealer
	}
}

func (h *cryptoSetup) GetOpener(level protocol.EncryptionLevel) (Opener, error) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	switch level {
	case protocol.EncryptionInitial:
		if h.initialOpener == nil {
			return nil, ErrKeysDropped
		}
		return h.initialOpener, nil
	case protocol.EncryptionHandshake:
		if h.handshakeOpener == nil {
			if h.initialOpener != nil {
				return nil, ErrOpenerNotYetAvailable
			}
			// if the initial opener is also not available, the keys were already dropped
			return nil, ErrKeysDropped
		}
		return h.handshakeOpener, nil
	case protocol.Encryption1RTT:
		if h.opener == nil {
			return nil, ErrOpenerNotYetAvailable
		}
		return h.opener, nil
	default:
		return nil, fmt.Errorf("CryptoSetup: no opener with encryption level %s", level)
	}
}

func (h *cryptoSetup) ConnectionState() tls.ConnectionState {
	cs := h.conn.ConnectionState()
	// h.conn is a qtls.Conn, which returns a qtls.ConnectionState.
	// qtls.ConnectionState is identical to the tls.ConnectionState.
	// It contains an unexported field which is used ExportKeyingMaterial().
	// The only way to return a tls.ConnectionState is to use unsafe.
	// In unsafe.go we check that the two objects are actually identical.
	return *(*tls.ConnectionState)(unsafe.Pointer(&cs))
}
