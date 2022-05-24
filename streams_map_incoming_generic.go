package quic

import (
	"sync"

	"github.com/For-ACGN/quic-bbr/internal/protocol"
	"github.com/For-ACGN/quic-bbr/internal/wire"
)

//go:generate genny -in $GOFILE -out streams_map_incoming_bidi.go gen "item=streamI Item=BidiStream streamTypeGeneric=protocol.StreamTypeBidi"
//go:generate genny -in $GOFILE -out streams_map_incoming_uni.go gen "item=receiveStreamI Item=UniStream streamTypeGeneric=protocol.StreamTypeUni"
type incomingItemsMap struct {
	mutex sync.RWMutex
	cond  sync.Cond

	streams map[protocol.StreamNum]item
	// When a stream is deleted before it was accepted, we can't delete it immediately.
	// We need to wait until the application accepts it, and delete it immediately then.
	streamsToDelete map[protocol.StreamNum]struct{} // used as a set

	nextStreamToAccept protocol.StreamNum // the next stream that will be returned by AcceptStream()
	nextStreamToOpen   protocol.StreamNum // the highest stream that the peer openend
	maxStream          protocol.StreamNum // the highest stream that the peer is allowed to open
	maxNumStreams      uint64             // maximum number of streams

	newStream        func(protocol.StreamNum) item
	queueMaxStreamID func(*wire.MaxStreamsFrame)
	// streamNumToID    func(protocol.StreamNum) protocol.StreamID // only used for generating errors

	closeErr error
}

func newIncomingItemsMap(
	newStream func(protocol.StreamNum) item,
	maxStreams uint64,
	queueControlFrame func(wire.Frame),
	// streamNumToID func(protocol.StreamNum) protocol.StreamID,
) *incomingItemsMap {
	m := &incomingItemsMap{
		streams:            make(map[protocol.StreamNum]item),
		streamsToDelete:    make(map[protocol.StreamNum]struct{}),
		maxStream:          protocol.StreamNum(maxStreams),
		maxNumStreams:      maxStreams,
		newStream:          newStream,
		nextStreamToOpen:   1,
		nextStreamToAccept: 1,
		queueMaxStreamID:   func(f *wire.MaxStreamsFrame) { queueControlFrame(f) },
		// streamNumToID:      streamNumToID,
	}
	m.cond.L = &m.mutex
	return m
}

func (m *incomingItemsMap) AcceptStream() (item, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	var num protocol.StreamNum
	var str item
	for {
		num = m.nextStreamToAccept
		var ok bool
		if m.closeErr != nil {
			return nil, m.closeErr
		}
		str, ok = m.streams[num]
		if ok {
			break
		}
		m.cond.Wait()
	}
	m.nextStreamToAccept++
	// If this stream was completed before being accepted, we can delete it now.
	if _, ok := m.streamsToDelete[num]; ok {
		delete(m.streamsToDelete, num)
		if err := m.deleteStream(num); err != nil {
			return nil, err
		}
	}
	return str, nil
}

func (m *incomingItemsMap) GetOrOpenStream(num protocol.StreamNum) (item, error) {
	m.mutex.RLock()
	if num > m.maxStream {
		m.mutex.RUnlock()
		return nil, streamError{
			message: "peer tried to open stream %d (current limit: %d)",
			nums:    []protocol.StreamNum{num, m.maxStream},
		}
	}
	// if the num is smaller than the highest we accepted
	// * this stream exists in the map, and we can return it, or
	// * this stream was already closed, then we can return the nil
	if num < m.nextStreamToOpen {
		var s item
		// If the stream was already queued for deletion, and is just waiting to be accepted, don't return it.
		if _, ok := m.streamsToDelete[num]; !ok {
			s = m.streams[num]
		}
		m.mutex.RUnlock()
		return s, nil
	}
	m.mutex.RUnlock()

	m.mutex.Lock()
	// no need to check the two error conditions from above again
	// * maxStream can only increase, so if the id was valid before, it definitely is valid now
	// * highestStream is only modified by this function
	for newNum := m.nextStreamToOpen; newNum <= num; newNum++ {
		m.streams[newNum] = m.newStream(newNum)
		m.cond.Signal()
	}
	m.nextStreamToOpen = num + 1
	s := m.streams[num]
	m.mutex.Unlock()
	return s, nil
}

func (m *incomingItemsMap) DeleteStream(num protocol.StreamNum) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	return m.deleteStream(num)
}

func (m *incomingItemsMap) deleteStream(num protocol.StreamNum) error {
	if _, ok := m.streams[num]; !ok {
		return streamError{
			message: "Tried to delete unknown stream %d",
			nums:    []protocol.StreamNum{num},
		}
	}

	// Don't delete this stream yet, if it was not yet accepted.
	// Just save it to streamsToDelete map, to make sure it is deleted as soon as it gets accepted.
	if num >= m.nextStreamToAccept {
		if _, ok := m.streamsToDelete[num]; ok {
			return streamError{
				message: "Tried to delete stream %d multiple times",
				nums:    []protocol.StreamNum{num},
			}
		}
		m.streamsToDelete[num] = struct{}{}
		return nil
	}

	delete(m.streams, num)
	// queue a MAX_STREAM_ID frame, giving the peer the option to open a new stream
	if m.maxNumStreams > uint64(len(m.streams)) {
		numNewStreams := m.maxNumStreams - uint64(len(m.streams))
		m.maxStream = m.nextStreamToOpen + protocol.StreamNum(numNewStreams) - 1
		m.queueMaxStreamID(&wire.MaxStreamsFrame{
			Type:         streamTypeGeneric,
			MaxStreamNum: m.maxStream,
		})
	}
	return nil
}

func (m *incomingItemsMap) CloseWithError(err error) {
	m.mutex.Lock()
	m.closeErr = err
	for _, str := range m.streams {
		str.closeForShutdown(err)
	}
	m.mutex.Unlock()
	m.cond.Broadcast()
}