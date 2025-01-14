package wire

import (
	"github.com/DrakenLibra/gt-bbr/internal/protocol"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Version Negotiation Packets", func() {
	It("writes", func() {
		srcConnID := protocol.ConnectionID{0xde, 0xad, 0xbe, 0xef, 0xca, 0xfe, 0x13, 0x37}
		destConnID := protocol.ConnectionID{1, 2, 3, 4, 5, 6, 7, 8}
		versions := []protocol.VersionNumber{1001, 1003}
		data, err := ComposeVersionNegotiation(destConnID, srcConnID, versions)
		Expect(err).ToNot(HaveOccurred())
		Expect(data[0] & 0x80).ToNot(BeZero())
		Expect(data[0] & 0x40).ToNot(BeZero())
		hdr, _, rest, err := ParsePacket(data, 4)
		Expect(err).ToNot(HaveOccurred())
		Expect(hdr.DestConnectionID).To(Equal(destConnID))
		Expect(hdr.SrcConnectionID).To(Equal(srcConnID))
		Expect(hdr.Version).To(BeZero())
		// the supported versions should include one reserved version number
		Expect(hdr.SupportedVersions).To(HaveLen(len(versions) + 1))
		for _, version := range versions {
			Expect(hdr.SupportedVersions).To(ContainElement(version))
		}
		Expect(rest).To(BeEmpty())
	})
})
