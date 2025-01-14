package wire

import (
	"bytes"

	"github.com/DrakenLibra/gt-bbr/internal/protocol"
	"github.com/DrakenLibra/gt-bbr/internal/utils"
)

// A StreamsBlockedFrame is a STREAMS_BLOCKED frame
type StreamsBlockedFrame struct {
	Type        protocol.StreamType
	StreamLimit protocol.StreamNum
}

func parseStreamsBlockedFrame(r *bytes.Reader, _ protocol.VersionNumber) (*StreamsBlockedFrame, error) {
	typeByte, err := r.ReadByte()
	if err != nil {
		return nil, err
	}

	f := &StreamsBlockedFrame{}
	switch typeByte {
	case 0x16:
		f.Type = protocol.StreamTypeBidi
	case 0x17:
		f.Type = protocol.StreamTypeUni
	}
	streamLimit, err := utils.ReadVarInt(r)
	if err != nil {
		return nil, err
	}
	f.StreamLimit = protocol.StreamNum(streamLimit)

	return f, nil
}

func (f *StreamsBlockedFrame) Write(b *bytes.Buffer, _ protocol.VersionNumber) error {
	switch f.Type {
	case protocol.StreamTypeBidi:
		b.WriteByte(0x16)
	case protocol.StreamTypeUni:
		b.WriteByte(0x17)
	}
	utils.WriteVarInt(b, uint64(f.StreamLimit))
	return nil
}

// Length of a written frame
func (f *StreamsBlockedFrame) Length(_ protocol.VersionNumber) protocol.ByteCount {
	return 1 + utils.VarIntLen(uint64(f.StreamLimit))
}
