package self_test

import (
	"fmt"
	"math/rand"
	"net"
	"time"

	quic "github.com/DrakenLibra/gt-bbr"
	quicproxy "github.com/DrakenLibra/gt-bbr/integrationtests/tools/proxy"
	"github.com/DrakenLibra/gt-bbr/internal/utils"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Stateless Resets", func() {
	connIDLens := []int{0, 10}

	for i := range connIDLens {
		connIDLen := connIDLens[i]

		It(fmt.Sprintf("sends and recognizes stateless resets, for %d byte connection IDs", connIDLen), func() {
			statelessResetKey := make([]byte, 32)
			rand.Read(statelessResetKey)
			serverConfig := &quic.Config{StatelessResetKey: statelessResetKey}

			ln, err := quic.ListenAddr("localhost:0", getTLSConfig(), serverConfig)
			Expect(err).ToNot(HaveOccurred())
			serverPort := ln.Addr().(*net.UDPAddr).Port

			closeServer := make(chan struct{})

			go func() {
				defer GinkgoRecover()
				sess, err := ln.Accept()
				Expect(err).ToNot(HaveOccurred())
				str, err := sess.OpenStream()
				Expect(err).ToNot(HaveOccurred())
				_, err = str.Write([]byte("foobar"))
				Expect(err).ToNot(HaveOccurred())
				<-closeServer
				ln.Close()
			}()

			drop := utils.AtomicBool{}

			proxy, err := quicproxy.NewQuicProxy("localhost:0", &quicproxy.Opts{
				RemoteAddr: fmt.Sprintf("localhost:%d", serverPort),
				DropPacket: func(d quicproxy.Direction, p uint64) bool {
					return drop.Get()
				},
			})
			Expect(err).ToNot(HaveOccurred())
			defer proxy.Close()

			sess, err := quic.DialAddr(
				fmt.Sprintf("localhost:%d", proxy.LocalPort()),
				getTLSClientConfig(),
				&quic.Config{
					ConnectionIDLength: connIDLen,
					IdleTimeout:        2 * time.Second,
				},
			)
			Expect(err).ToNot(HaveOccurred())
			str, err := sess.AcceptStream()
			Expect(err).ToNot(HaveOccurred())
			data := make([]byte, 6)
			_, err = str.Read(data)
			Expect(err).ToNot(HaveOccurred())
			Expect(data).To(Equal([]byte("foobar")))

			// make sure that the CONNECTION_CLOSE is dropped
			drop.Set(true)
			close(closeServer)
			time.Sleep(100 * time.Millisecond)

			ln2, err := quic.ListenAddr(
				fmt.Sprintf("localhost:%d", serverPort),
				getTLSConfig(),
				serverConfig,
			)
			Expect(err).ToNot(HaveOccurred())
			drop.Set(false)

			acceptStopped := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				_, err := ln2.Accept()
				Expect(err).To(HaveOccurred())
				close(acceptStopped)
			}()

			// trigger something (not too small) to be sent,
			// so that we receive the stateless reset
			_, err = str.Write([]byte("Lorem ipsum dolor sit amet."))
			Expect(err).ToNot(HaveOccurred())
			_, err = str.Read([]byte{0})
			Expect(err).To(MatchError("INTERNAL_ERROR: received a stateless reset"))

			Expect(ln2.Close()).To(Succeed())
			Eventually(acceptStopped).Should(BeClosed())
		})
	}
})
