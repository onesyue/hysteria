package integration_tests

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/core/v2/internal/integration_tests/mocks"
	"github.com/apernet/hysteria/core/v2/server"
)

// recordingOutbound records which entry point each TCP request used.
type recordingOutbound struct {
	mu      sync.Mutex
	plain   []string
	authed  [][2]string
	withAPI bool
}

func (o *recordingOutbound) dial() (net.Conn, error) {
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(b, b); _ = b.Close() }()
	return a, nil
}

func (o *recordingOutbound) TCP(reqAddr string) (net.Conn, error) {
	o.mu.Lock()
	o.plain = append(o.plain, reqAddr)
	o.mu.Unlock()
	return o.dial()
}

func (o *recordingOutbound) UDP(string) (server.UDPConn, error) { return nil, errors.New("no udp") }
func (o *recordingOutbound) CheckUDP(string) error              { return nil }

type authedRecordingOutbound struct{ *recordingOutbound }

func (o authedRecordingOutbound) TCPAuthenticated(authID, reqAddr string) (net.Conn, error) {
	o.mu.Lock()
	o.authed = append(o.authed, [2]string{authID, reqAddr})
	o.mu.Unlock()
	return o.dial()
}

func runAuthenticatedOutbound(t *testing.T, ob server.Outbound) {
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
	conn, err := c.TCP("example.test:443")
	assert.NoError(t, err)
	_, err = conn.Write([]byte("ping"))
	assert.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	assert.NoError(t, err)
	assert.Equal(t, "ping", string(buf))
	_ = conn.Close()
}

// TestAuthenticatedOutboundReceivesAuthID: an Outbound with the optional
// extension gets the authenticated ID with every TCP request, and the plain
// entry point is not used.
func TestAuthenticatedOutboundReceivesAuthID(t *testing.T) {
	rec := &recordingOutbound{}
	runAuthenticatedOutbound(t, authedRecordingOutbound{rec})
	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, [][2]string{{"user-42", "example.test:443"}}, rec.authed)
	assert.Empty(t, rec.plain)
}

// TestPlainOutboundUnchanged: an Outbound without the extension keeps the
// upstream TCP(reqAddr) call.
func TestPlainOutboundUnchanged(t *testing.T) {
	rec := &recordingOutbound{}
	runAuthenticatedOutbound(t, rec)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, []string{"example.test:443"}, rec.plain)
	assert.Empty(t, rec.authed)
}
