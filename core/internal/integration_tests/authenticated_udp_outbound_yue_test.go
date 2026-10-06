package integration_tests

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/core/v2/internal/integration_tests/mocks"
	"github.com/apernet/hysteria/core/v2/server"
)

// echoUDPConn is a session socket that echoes every datagram back from the
// address it was sent to.
type echoUDPConn struct {
	ch     chan [2]string
	once   sync.Once
	closed chan struct{}
}

func newEchoUDPConn() *echoUDPConn {
	return &echoUDPConn{ch: make(chan [2]string, 8), closed: make(chan struct{})}
}

func (c *echoUDPConn) ReadFrom(b []byte) (int, string, error) {
	select {
	case m := <-c.ch:
		return copy(b, m[0]), m[1], nil
	case <-c.closed:
		return 0, "", net.ErrClosed
	}
}

func (c *echoUDPConn) WriteTo(b []byte, addr string) (int, error) {
	select {
	case c.ch <- [2]string{string(b), addr}:
	case <-c.closed:
		return 0, net.ErrClosed
	}
	return len(b), nil
}

func (c *echoUDPConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// recordingUDPOutbound records which entry point each UDP session used.
type recordingUDPOutbound struct {
	mu     sync.Mutex
	plain  []string
	authed [][2]string
}

func (o *recordingUDPOutbound) TCP(string) (net.Conn, error) { return nil, errors.New("no tcp") }
func (o *recordingUDPOutbound) CheckUDP(string) error        { return nil }

func (o *recordingUDPOutbound) UDP(reqAddr string) (server.UDPConn, error) {
	o.mu.Lock()
	o.plain = append(o.plain, reqAddr)
	o.mu.Unlock()
	return newEchoUDPConn(), nil
}

type authedRecordingUDPOutbound struct{ *recordingUDPOutbound }

func (o authedRecordingUDPOutbound) UDPAuthenticated(authID, reqAddr string) (server.UDPConn, error) {
	o.mu.Lock()
	o.authed = append(o.authed, [2]string{authID, reqAddr})
	o.mu.Unlock()
	return newEchoUDPConn(), nil
}

func runAuthenticatedUDPOutbound(t *testing.T, ob server.Outbound) {
	t.Helper()
	udpConn, udpAddr, err := serverConn()
	assert.NoError(t, err)
	auth := mocks.NewMockAuthenticator(t)
	auth.EXPECT().Authenticate(mock.Anything, mock.Anything, mock.Anything).Return(true, "user-42")
	s, err := server.NewServer(&server.Config{
		TLSConfig:     serverTLSConfig(),
		Conn:          udpConn,
		Outbound:      ob,
		Authenticator: auth,
	})
	assert.NoError(t, err)
	defer s.Close()
	go s.Serve()

	c, _, err := client.NewClient(&client.Config{
		ServerAddr: udpAddr,
		TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
	})
	assert.NoError(t, err)
	defer c.Close()
	conn, err := c.UDP()
	assert.NoError(t, err)
	defer conn.Close()
	// Two destinations in one session: the session socket is opened once,
	// for the first destination, and serves the second as well.
	for _, addr := range []string{"192.0.2.1:6881", "192.0.2.2:53"} {
		assert.NoError(t, conn.Send([]byte("ping"), addr))
		done := make(chan struct{})
		var data []byte
		var from string
		go func() {
			data, from, err = conn.Receive()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("no echo")
		}
		assert.NoError(t, err)
		assert.Equal(t, "ping", string(data))
		assert.Equal(t, addr, from)
	}
}

// TestAuthenticatedUDPOutboundReceivesAuthID: an Outbound with the optional
// UDP extension opens the session socket with the authenticated ID (once per
// session, for its first destination), and the plain entry point is not used.
func TestAuthenticatedUDPOutboundReceivesAuthID(t *testing.T) {
	rec := &recordingUDPOutbound{}
	runAuthenticatedUDPOutbound(t, authedRecordingUDPOutbound{rec})
	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, [][2]string{{"user-42", "192.0.2.1:6881"}}, rec.authed)
	assert.Empty(t, rec.plain)
}

// TestPlainUDPOutboundUnchanged: an Outbound without the UDP extension keeps
// the upstream UDP(reqAddr) call.
func TestPlainUDPOutboundUnchanged(t *testing.T) {
	rec := &recordingUDPOutbound{}
	runAuthenticatedUDPOutbound(t, rec)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, []string{"192.0.2.1:6881"}, rec.plain)
	assert.Empty(t, rec.authed)
}
