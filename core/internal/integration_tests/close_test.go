package integration_tests

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/core/v2/errors"
	"github.com/apernet/hysteria/core/v2/internal/integration_tests/mocks"
	"github.com/apernet/hysteria/core/v2/server"
)

// TestClientServerTCPClose tests whether the client/server propagates the close of a connection correctly.
// Closing one side of the connection should close the other side as well.
func TestClientServerTCPClose(t *testing.T) {
	// Create server
	udpConn, udpAddr, err := serverConn()
	assert.NoError(t, err)
	serverOb := mocks.NewMockOutbound(t)
	auth := mocks.NewMockAuthenticator(t)
	auth.EXPECT().Authenticate(mock.Anything, mock.Anything, mock.Anything).Return(true, "nobody")
	s, err := server.NewServer(&server.Config{
		TLSConfig:     serverTLSConfig(),
		Conn:          udpConn,
		Outbound:      serverOb,
		Authenticator: auth,
	})
	assert.NoError(t, err)
	defer s.Close()
	go s.Serve()

	// Create client
	c, _, err := client.NewClient(&client.Config{
		ServerAddr: udpAddr,
		TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
	})
	assert.NoError(t, err)
	defer c.Close()

	addr := "hi-and-goodbye:2333"

	// Test close from client side:
	// Client creates a connection, writes something, then closes it.
	// Server outbound connection should write the same thing, then close.
	sobConn := mocks.NewMockConn(t)
	sobConnCh := make(chan struct{}) // For close signal only
	sobConnChCloseFunc := sync.OnceFunc(func() { close(sobConnCh) })
	sobConn.EXPECT().Read(mock.Anything).RunAndReturn(func(bs []byte) (int, error) {
		<-sobConnCh
		return 0, io.EOF
	})
	sobConn.EXPECT().Write([]byte("happy")).Return(5, nil)
	sobConn.EXPECT().Close().RunAndReturn(func() error {
		sobConnChCloseFunc()
		return nil
	})
	serverOb.EXPECT().TCP(addr).Return(sobConn, nil).Once()
	conn, err := c.TCP(addr)
	assert.NoError(t, err)
	_, err = conn.Write([]byte("happy"))
	assert.NoError(t, err)
	err = conn.Close()
	assert.NoError(t, err)
	time.Sleep(1 * time.Second)
	mock.AssertExpectationsForObjects(t, sobConn, serverOb)

	// Test close from server side:
	// Client creates a connection.
	// Server outbound connection reads something, then closes.
	// Client connection should read the same thing, then close.
	sobConn = mocks.NewMockConn(t)
	sobConnCh2 := make(chan []byte, 1)
	sobConn.EXPECT().Read(mock.Anything).RunAndReturn(func(bs []byte) (int, error) {
		d := <-sobConnCh2
		if d == nil {
			return 0, io.EOF
		} else {
			return copy(bs, d), nil
		}
	})
	sobConn.EXPECT().Close().Return(nil)
	serverOb.EXPECT().TCP(addr).Return(sobConn, nil).Once()
	conn, err = c.TCP(addr)
	assert.NoError(t, err)
	sobConnCh2 <- []byte("happy")
	close(sobConnCh2)
	bs, err := io.ReadAll(conn)
	assert.NoError(t, err)
	assert.Equal(t, "happy", string(bs))
}

// TestClientServerUDPIdleTimeout tests whether the server's UDP idle timeout works correctly.
func TestClientServerUDPIdleTimeout(t *testing.T) {
	// Create server
	udpConn, udpAddr, err := serverConn()
	assert.NoError(t, err)
	serverOb := mocks.NewMockOutbound(t)
	auth := mocks.NewMockAuthenticator(t)
	auth.EXPECT().Authenticate(mock.Anything, mock.Anything, mock.Anything).Return(true, "nobody")
	eventLogger := mocks.NewMockEventLogger(t)
	eventLogger.EXPECT().Connect(mock.Anything, "nobody", mock.Anything).Once()
	eventLogger.EXPECT().Disconnect(mock.Anything, "nobody", mock.Anything).Maybe() // Depends on the timing, don't care
	s, err := server.NewServer(&server.Config{
		TLSConfig:      serverTLSConfig(),
		Conn:           udpConn,
		Outbound:       serverOb,
		UDPIdleTimeout: 2 * time.Second,
		Authenticator:  auth,
		EventLogger:    eventLogger,
	})
	assert.NoError(t, err)
	defer s.Close()
	go s.Serve()

	// Create client
	c, _, err := client.NewClient(&client.Config{
		ServerAddr: udpAddr,
		TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
	})
	assert.NoError(t, err)
	defer c.Close()

	addr := "spy.x.family:2023"

	// On the client side, create a UDP session and send a packet every 1 second,
	// 4 packets in total. The server should have one UDP session and receive all
	// 4 packets. Then the UDP connection on the server side will receive a packet
	// every 1 second, 4 packets in total. The client session should receive all
	// 4 packets. Then the session will be idle for 3 seconds - should be enough
	// to trigger the server's UDP idle timeout.
	sobConn := mocks.NewMockUDPConn(t)
	sobConnCh := make(chan []byte, 1)
	sobConnChCloseFunc := sync.OnceFunc(func() { close(sobConnCh) })
	sobConn.EXPECT().ReadFrom(mock.Anything).RunAndReturn(func(bs []byte) (int, string, error) {
		d := <-sobConnCh
		if d == nil {
			return 0, "", io.EOF
		} else {
			return copy(bs, d), addr, nil
		}
	})
	sobConn.EXPECT().WriteTo([]byte("happy"), addr).Return(5, nil).Times(4)
	serverOb.EXPECT().UDP(addr).Return(sobConn, nil).Once()
	eventLogger.EXPECT().UDPRequest(mock.Anything, mock.Anything, uint32(1), addr).Once()
	cu, err := c.UDP()
	assert.NoError(t, err)
	// Client sends 4 packets
	for i := 0; i < 4; i++ {
		err = cu.Send([]byte("happy"), addr)
		assert.NoError(t, err)
		time.Sleep(1 * time.Second)
	}
	// Client receives 4 packets
	go func() {
		for i := 0; i < 4; i++ {
			sobConnCh <- []byte("sad")
			time.Sleep(1 * time.Second)
		}
	}()
	for i := 0; i < 4; i++ {
		bs, rAddr, err := cu.Receive()
		assert.NoError(t, err)
		assert.Equal(t, "sad", string(bs))
		assert.Equal(t, addr, rAddr)
	}
	// Now we wait for 3 seconds, the server should close the UDP session.
	sobConn.EXPECT().Close().RunAndReturn(func() error {
		sobConnChCloseFunc()
		return nil
	})
	eventLogger.EXPECT().UDPError(mock.Anything, mock.Anything, uint32(1), nil).Once()
	time.Sleep(3 * time.Second)
}

// TestClientServerClientShutdown tests whether the server can handle the client's shutdown correctly.
func TestClientServerClientShutdown(t *testing.T) {
	// Create server
	udpConn, udpAddr, err := serverConn()
	assert.NoError(t, err)
	auth := mocks.NewMockAuthenticator(t)
	auth.EXPECT().Authenticate(mock.Anything, mock.Anything, mock.Anything).Return(true, "nobody")
	eventLogger := mocks.NewMockEventLogger(t)
	eventLogger.EXPECT().Connect(mock.Anything, "nobody", mock.Anything).Once()
	s, err := server.NewServer(&server.Config{
		TLSConfig:     serverTLSConfig(),
		Conn:          udpConn,
		Authenticator: auth,
		EventLogger:   eventLogger,
	})
	assert.NoError(t, err)
	defer s.Close()
	go s.Serve()

	// Create client
	c, _, err := client.NewClient(&client.Config{
		ServerAddr: udpAddr,
		TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
	})
	assert.NoError(t, err)

	// Close the client - expect disconnect event on the server side.
	// Since client.Close() sends HTTP3 ErrCodeNoError, the error should be nil.
	eventLogger.EXPECT().Disconnect(mock.Anything, "nobody", nil).Once()
	_ = c.Close()
	time.Sleep(1 * time.Second)
}

// TestClientServerServerShutdown tests whether the client can handle the server's shutdown correctly.
func TestClientServerServerShutdown(t *testing.T) {
	// Create server
	udpConn, udpAddr, err := serverConn()
	require.NoError(t, err)
	t.Cleanup(func() { _ = udpConn.Close() })
	auth := mocks.NewMockAuthenticator(t)
	auth.EXPECT().Authenticate(mock.Anything, mock.Anything, mock.Anything).Return(true, "nobody")
	s, err := server.NewServer(&server.Config{
		TLSConfig:     serverTLSConfig(),
		Conn:          udpConn,
		Authenticator: auth,
	})
	require.NoError(t, err)
	defer s.Close()
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- s.Serve() }()

	// Create client
	c, _, err := client.NewClient(&client.Config{
		ServerAddr: udpAddr,
		TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
		QUICConfig: client.QUICConfig{
			MaxIdleTimeout: 4 * time.Second,
		},
	})
	require.NoError(t, err)
	defer c.Close()

	// Close the server - expect the client to return ClosedError for both TCP & UDP calls.
	closeStarted := time.Now()
	require.NoError(t, s.Close())
	require.Less(t, time.Since(closeStarted), 5*time.Second)

	select {
	case err = <-serveErrCh:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after Close")
	}

	// Transport.Close alone is abrupt and leaves the peer waiting for its idle
	// timeout. A bounded call pins the production contract that server shutdown
	// notifies established clients immediately.
	tcpErrCh := make(chan error, 1)
	go func() {
		_, tcpErr := c.TCP("whatever")
		tcpErrCh <- tcpErr
	}()
	select {
	case err = <-tcpErrCh:
		_, ok := err.(errors.ClosedError)
		require.True(t, ok, "TCP error = %T: %v", err, err)
	case <-time.After(5 * time.Second):
		t.Fatal("client did not observe server shutdown")
	}

	require.Eventually(t, func() bool {
		_, udpErr := c.UDP()
		_, ok := udpErr.(errors.ClosedError)
		return ok
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, c.Close())

	// Close is also the socket ownership barrier: the exact same address must be
	// reusable immediately, without relying on a later idle timeout or GC.
	rebound, err := net.ListenUDP("udp", udpAddr.(*net.UDPAddr))
	require.NoError(t, err)
	require.NoError(t, rebound.Close())
}
