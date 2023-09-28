package http3

import (
	"io"

	quic "github.com/DrakenLibra/gt-bbr"
)

type responseBody struct {
	quic.Stream
}

var _ io.ReadCloser = &responseBody{}

func (rb *responseBody) Close() error {
	rb.Stream.CancelRead(0)
	return nil
}
