package wire

import (
	"bytes"

	"github.com/DrakenLibra/gt-bbr/internal/protocol"
	"github.com/DrakenLibra/gt-bbr/internal/utils"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("MAX_STREAMS frame", func() {
	Context("parsing", func() {
		It("accepts a frame for a bidirectional stream", func() {
			data := []byte{0x12}
			data = append(data, encodeVarInt(0xdecaf)...)
			b := bytes.NewReader(data)
			f, err := parseMaxStreamsFrame(b, protocol.VersionWhatever)
			Expect(err).ToNot(HaveOccurred())
			Expect(f.Type).To(Equal(protocol.StreamTypeBidi))
			Expect(f.MaxStreamNum).To(BeEquivalentTo(0xdecaf))
			Expect(b.Len()).To(BeZero())
		})

		It("accepts a frame for a bidirectional stream", func() {
			data := []byte{0x13}
			data = append(data, encodeVarInt(0xdecaf)...)
			b := bytes.NewReader(data)
			f, err := parseMaxStreamsFrame(b, protocol.VersionWhatever)
			Expect(err).ToNot(HaveOccurred())
			Expect(f.Type).To(Equal(protocol.StreamTypeUni))
			Expect(f.MaxStreamNum).To(BeEquivalentTo(0xdecaf))
			Expect(b.Len()).To(BeZero())
		})

		It("errors on EOFs", func() {
			data := []byte{0x1d}
			data = append(data, encodeVarInt(0xdeadbeefcafe13)...)
			_, err := parseMaxStreamsFrame(bytes.NewReader(data), protocol.VersionWhatever)
			Expect(err).NotTo(HaveOccurred())
			for i := range data {
				_, err := parseMaxStreamsFrame(bytes.NewReader(data[0:i]), protocol.VersionWhatever)
				Expect(err).To(HaveOccurred())
			}
		})
	})

	Context("writing", func() {
		It("for a bidirectional stream", func() {
			f := &MaxStreamsFrame{
				Type:         protocol.StreamTypeBidi,
				MaxStreamNum: 0xdeadbeef,
			}
			b := &bytes.Buffer{}
			Expect(f.Write(b, protocol.VersionWhatever)).To(Succeed())
			expected := []byte{0x12}
			expected = append(expected, encodeVarInt(0xdeadbeef)...)
			Expect(b.Bytes()).To(Equal(expected))
		})

		It("for a unidirectional stream", func() {
			f := &MaxStreamsFrame{
				Type:         protocol.StreamTypeUni,
				MaxStreamNum: 0xdecafbad,
			}
			b := &bytes.Buffer{}
			Expect(f.Write(b, protocol.VersionWhatever)).To(Succeed())
			expected := []byte{0x13}
			expected = append(expected, encodeVarInt(0xdecafbad)...)
			Expect(b.Bytes()).To(Equal(expected))
		})

		It("has the correct min length", func() {
			frame := MaxStreamsFrame{MaxStreamNum: 0x1337}
			Expect(frame.Length(protocol.VersionWhatever)).To(Equal(1 + utils.VarIntLen(0x1337)))
		})
	})
})
