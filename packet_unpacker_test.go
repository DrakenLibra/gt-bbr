package quic

import (
	"bytes"
	"errors"

	"github.com/DrakenLibra/gt-bbr/internal/handshake"
	"github.com/DrakenLibra/gt-bbr/internal/mocks"
	"github.com/DrakenLibra/gt-bbr/internal/protocol"
	"github.com/DrakenLibra/gt-bbr/internal/wire"
	"github.com/golang/mock/gomock"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Packet Unpacker", func() {
	const version = protocol.VersionTLS
	var (
		unpacker *packetUnpacker
		cs       *mocks.MockCryptoSetup
		connID   = protocol.ConnectionID{0xde, 0xad, 0xbe, 0xef}
		payload  = []byte("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua.")
	)

	getHeader := func(extHdr *wire.ExtendedHeader) (*wire.Header, []byte) {
		buf := &bytes.Buffer{}
		ExpectWithOffset(1, extHdr.Write(buf, protocol.VersionWhatever)).To(Succeed())
		hdrLen := buf.Len()
		if extHdr.Length > protocol.ByteCount(extHdr.PacketNumberLen) {
			buf.Write(make([]byte, int(extHdr.Length)-int(extHdr.PacketNumberLen)))
		}
		hdr, _, _, err := wire.ParsePacket(buf.Bytes(), connID.Len())
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		return hdr, buf.Bytes()[:hdrLen]
	}

	BeforeEach(func() {
		cs = mocks.NewMockCryptoSetup(mockCtrl)
		unpacker = newPacketUnpacker(cs, version).(*packetUnpacker)
	})

	It("errors when the packet is too small to obtain the header decryption sample", func() {
		extHdr := &wire.ExtendedHeader{
			Header:          wire.Header{DestConnectionID: connID},
			PacketNumber:    1337,
			PacketNumberLen: protocol.PacketNumberLen2,
		}
		hdr, hdrRaw := getHeader(extHdr)
		data := append(hdrRaw, make([]byte, 2 /* fill up packet number */ +15 /* need 16 bytes */)...)
		opener := mocks.NewMockOpener(mockCtrl)
		cs.EXPECT().GetOpener(protocol.Encryption1RTT).Return(opener, nil)
		_, err := unpacker.Unpack(hdr, data)
		Expect(err).To(MatchError("Packet too small. Expected at least 20 bytes after the header, got 19"))
	})

	It("opens Initial packets", func() {
		extHdr := &wire.ExtendedHeader{
			Header: wire.Header{
				IsLongHeader:     true,
				Type:             protocol.PacketTypeInitial,
				Length:           3 + 6, // packet number len + payload
				DestConnectionID: connID,
				Version:          version,
			},
			PacketNumber:    2,
			PacketNumberLen: 3,
		}
		hdr, hdrRaw := getHeader(extHdr)
		opener := mocks.NewMockOpener(mockCtrl)
		cs.EXPECT().GetOpener(protocol.EncryptionInitial).Return(opener, nil)
		opener.EXPECT().DecryptHeader(gomock.Any(), gomock.Any(), gomock.Any())
		opener.EXPECT().Open(gomock.Any(), payload, extHdr.PacketNumber, hdrRaw).Return([]byte("decrypted"), nil)
		packet, err := unpacker.Unpack(hdr, append(hdrRaw, payload...))
		Expect(err).ToNot(HaveOccurred())
		Expect(packet.encryptionLevel).To(Equal(protocol.EncryptionInitial))
		Expect(packet.data).To(Equal([]byte("decrypted")))
	})

	It("returns the error when getting the sealer fails", func() {
		extHdr := &wire.ExtendedHeader{
			Header:          wire.Header{DestConnectionID: connID},
			PacketNumber:    0x1337,
			PacketNumberLen: 2,
		}
		hdr, hdrRaw := getHeader(extHdr)
		cs.EXPECT().GetOpener(protocol.Encryption1RTT).Return(nil, handshake.ErrOpenerNotYetAvailable)
		_, err := unpacker.Unpack(hdr, append(hdrRaw, payload...))
		Expect(err).To(MatchError(handshake.ErrOpenerNotYetAvailable))
	})

	It("returns the error when unpacking fails", func() {
		extHdr := &wire.ExtendedHeader{
			Header: wire.Header{
				IsLongHeader:     true,
				Type:             protocol.PacketTypeHandshake,
				Length:           3, // packet number len
				DestConnectionID: connID,
				Version:          version,
			},
			PacketNumber:    2,
			PacketNumberLen: 3,
		}
		hdr, hdrRaw := getHeader(extHdr)
		opener := mocks.NewMockOpener(mockCtrl)
		cs.EXPECT().GetOpener(protocol.EncryptionHandshake).Return(opener, nil)
		opener.EXPECT().DecryptHeader(gomock.Any(), gomock.Any(), gomock.Any())
		opener.EXPECT().Open(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, errors.New("test err"))
		_, err := unpacker.Unpack(hdr, append(hdrRaw, payload...))
		Expect(err).To(MatchError("test err"))
	})

	It("decrypts the header", func() {
		extHdr := &wire.ExtendedHeader{
			Header: wire.Header{
				IsLongHeader:     true,
				Type:             protocol.PacketTypeHandshake,
				Length:           3, // packet number len
				DestConnectionID: connID,
				Version:          version,
			},
			PacketNumber:    0x1337,
			PacketNumberLen: 2,
		}
		hdr, hdrRaw := getHeader(extHdr)
		origHdrRaw := append([]byte{}, hdrRaw...) // save a copy of the header
		firstHdrByte := hdrRaw[0]
		hdrRaw[0] ^= 0xff             // invert the first byte
		hdrRaw[len(hdrRaw)-2] ^= 0xff // invert the packet number
		hdrRaw[len(hdrRaw)-1] ^= 0xff // invert the packet number
		Expect(hdrRaw[0]).ToNot(Equal(firstHdrByte))
		opener := mocks.NewMockOpener(mockCtrl)
		cs.EXPECT().GetOpener(protocol.EncryptionHandshake).Return(opener, nil)
		gomock.InOrder(
			// we're using a 2 byte packet number, so the sample starts at the 3rd payload byte
			opener.EXPECT().DecryptHeader(
				[]byte{3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18},
				&hdrRaw[0],
				append(hdrRaw[len(hdrRaw)-2:], []byte{1, 2}...)).Do(func(_ []byte, firstByte *byte, pnBytes []byte) {
				*firstByte ^= 0xff // invert the first byte back
				for i := range pnBytes {
					pnBytes[i] ^= 0xff // invert the packet number bytes
				}
			}),
			opener.EXPECT().Open(gomock.Any(), gomock.Any(), protocol.PacketNumber(0x1337), origHdrRaw).Return([]byte{0}, nil),
		)
		data := hdrRaw
		for i := 1; i <= 100; i++ {
			data = append(data, uint8(i))
		}
		packet, err := unpacker.Unpack(hdr, data)
		Expect(err).ToNot(HaveOccurred())
		Expect(packet.packetNumber).To(Equal(protocol.PacketNumber(0x1337)))
	})

	It("decodes the packet number", func() {
		firstHdr := &wire.ExtendedHeader{
			Header:          wire.Header{DestConnectionID: connID},
			PacketNumber:    0x1337,
			PacketNumberLen: 2,
		}
		opener := mocks.NewMockOpener(mockCtrl)
		cs.EXPECT().GetOpener(protocol.Encryption1RTT).Return(opener, nil).Times(2)
		opener.EXPECT().DecryptHeader(gomock.Any(), gomock.Any(), gomock.Any())
		opener.EXPECT().Open(gomock.Any(), gomock.Any(), firstHdr.PacketNumber, gomock.Any()).Return([]byte{0}, nil)
		hdr, hdrRaw := getHeader(firstHdr)
		packet, err := unpacker.Unpack(hdr, append(hdrRaw, payload...))
		Expect(err).ToNot(HaveOccurred())
		Expect(packet.packetNumber).To(Equal(protocol.PacketNumber(0x1337)))
		// the real packet number is 0x1338, but only the last byte is sent
		secondHdr := &wire.ExtendedHeader{
			Header:          wire.Header{DestConnectionID: connID},
			PacketNumber:    0x38,
			PacketNumberLen: 1,
		}
		// expect the call with the decoded packet number
		opener.EXPECT().DecryptHeader(gomock.Any(), gomock.Any(), gomock.Any())
		opener.EXPECT().Open(gomock.Any(), gomock.Any(), protocol.PacketNumber(0x1338), gomock.Any()).Return([]byte{0}, nil)
		hdr, hdrRaw = getHeader(secondHdr)
		packet, err = unpacker.Unpack(hdr, append(hdrRaw, payload...))
		Expect(err).ToNot(HaveOccurred())
		Expect(packet.packetNumber).To(Equal(protocol.PacketNumber(0x1338)))
	})
})
