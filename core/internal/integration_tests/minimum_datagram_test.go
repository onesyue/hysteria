package integration_tests

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/core/v2/server"
	"github.com/stretchr/testify/require"
)

// mtuPacketConn models a path that silently drops oversized DF datagrams.
// The limit accounts for the outer IP/UDP header and Salamander's 8-byte salt;
// TLS, QUIC, Hysteria authentication, stream transfer and PMTUD remain real.
type mtuPacketConn struct {
	net.PacketConn
	limit   int
	dropped atomic.Int64
}

func (c *mtuPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if len(p) > c.limit {
		c.dropped.Add(1)
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}

type mtuConnFactory struct{ conn net.PacketConn }

func (f mtuConnFactory) New(net.Addr) (net.PacketConn, error) { return f.conn, nil }

type mtuAuthenticator struct{ called atomic.Int64 }

func (a *mtuAuthenticator) Authenticate(_ net.Addr, password string, _ uint64) (bool, string) {
	a.called.Add(1)
	return password == "native-pmtu-test", "pmtu-user"
}

func TestClientServerMinimumDatagram(t *testing.T) {
	for _, ipHeader := range []int{20, 40} {
		t.Run(fmt.Sprintf("1280-IP-header-%d", ipHeader), func(t *testing.T) {
			// IPv4 permits 1244 inner bytes; IPv6 permits 1224. Both can carry
			// RFC 9000's 1200-byte minimum plus the actual encapsulation overhead.
			limit := 1280 - ipHeader - 8 - 8
			serverUDP, err := net.ListenPacket("udp4", "127.0.0.1:0")
			require.NoError(t, err)
			serverWire := &mtuPacketConn{PacketConn: serverUDP, limit: limit}
			auth := &mtuAuthenticator{}
			s, err := server.NewServer(&server.Config{TLSConfig: serverTLSConfig(), Conn: serverWire, Authenticator: auth})
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() })
			go func() { _ = s.Serve() }()
			tcpListener, err := net.Listen("tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			echo := &tcpEchoServer{Listener: tcpListener}
			t.Cleanup(func() { _ = echo.Close() })
			go func() { _ = echo.Serve() }()
			clientUDP, err := net.ListenPacket("udp4", "127.0.0.1:0")
			require.NoError(t, err)
			clientWire := &mtuPacketConn{PacketConn: clientUDP, limit: limit}
			t.Cleanup(func() { _ = clientWire.Close() })
			// Also bounds the old-source negative control: a permanently oversized
			// Initial must fail the test instead of hanging the test process.
			deadline := time.AfterFunc(4*time.Second, func() { _ = clientWire.Close() })
			defer deadline.Stop()
			c, _, err := client.NewClient(&client.Config{ConnFactory: mtuConnFactory{clientWire}, ServerAddr: serverUDP.LocalAddr(), Auth: "native-pmtu-test", TLSConfig: client.TLSConfig{InsecureSkipVerify: true}})
			require.NoError(t, err, "real handshake must fit a 1280-byte encapsulated path")
			deadline.Stop()
			t.Cleanup(func() { _ = c.Close() })
			require.EqualValues(t, 1, auth.called.Load())
			stream, err := c.TCP(tcpListener.Addr().String())
			require.NoError(t, err)
			defer stream.Close()
			require.NoError(t, stream.SetDeadline(time.Now().Add(5*time.Second)))
			payload := bytes.Repeat([]byte("native-small-initial\x00"), 8192)
			sent := make(chan error, 1)
			go func() { _, err := stream.Write(payload); sent <- err }()
			received := make([]byte, len(payload))
			_, err = io.ReadFull(stream, received)
			require.NoError(t, err)
			require.NoError(t, <-sent)
			require.Equal(t, payload, received)
			t.Logf("authenticated bytes=%d client_oversize_probes=%d server_oversize_probes=%d", len(payload), clientWire.dropped.Load(), serverWire.dropped.Load())
		})
	}
}
