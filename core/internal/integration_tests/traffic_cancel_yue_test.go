package integration_tests

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/core/v2/server"
)

type cancellableTrafficLogger struct {
	verdictLogger
	wait    atomic.Bool
	entered chan struct{}
	exited  chan struct{}
	rescue  chan struct{}
}

func (l *cancellableTrafficLogger) block(ctx context.Context) uint8 {
	if !l.wait.Load() {
		return server.TrafficAccept
	}
	l.entered <- struct{}{}
	select {
	case <-ctx.Done():
	case <-l.rescue:
	}
	l.exited <- struct{}{}
	return server.TrafficReject
}

func (l *cancellableTrafficLogger) LogStreamTraffic(string, uint64, uint64) uint8 {
	return l.block(context.Background())
}

func (l *cancellableTrafficLogger) LogSentDatagramTraffic(string, uint64, uint64) uint8 {
	return l.block(context.Background())
}

func (l *cancellableTrafficLogger) LogStreamTrafficContext(ctx context.Context, _ string, _, _ uint64) uint8 {
	return l.block(ctx)
}

func (l *cancellableTrafficLogger) LogSentDatagramTrafficContext(ctx context.Context, _ string, _, _ uint64) uint8 {
	return l.block(ctx)
}

func waitTrafficCancellationSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out: %s", what)
	}
}

// Both copy workers can be inside the limiter rather than socket I/O when a
// peer closes. Socket closure alone cannot free them, and server.Close joins
// the handler that is waiting for them. Exercise real QUIC stream and whole
// connection close as well as the server's lifecycle barrier.
func TestRealQUICCloseCancelsBothTrafficWaits(t *testing.T) {
	for _, closeSide := range []string{"stream", "client", "server"} {
		t.Run(closeSide, func(t *testing.T) {
			logger := &cancellableTrafficLogger{entered: make(chan struct{}, 2), exited: make(chan struct{}, 2), rescue: make(chan struct{})}
			logger.wait.Store(true)
			release := sync.OnceFunc(func() { close(logger.rescue) })
			defer release()
			udp, addr, err := serverConn()
			if err != nil {
				t.Fatal(err)
			}
			s, err := server.NewServer(&server.Config{TLSConfig: serverTLSConfig(), Conn: udp, Authenticator: acceptAll{}, TrafficLogger: logger})
			if err != nil {
				udp.Close()
				t.Fatal(err)
			}
			go s.Serve()
			defer func() { release(); s.Close() }()
			c, _, err := client.NewClient(&client.Config{ServerAddr: addr, TLSConfig: client.TLSConfig{InsecureSkipVerify: true}})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				conn.Write([]byte("downstream"))
				io.Copy(io.Discard, conn)
			}()
			stream, err := c.TCP(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if _, err := stream.Write([]byte("upstream")); err != nil {
				t.Fatal(err)
			}
			waitTrafficCancellationSignal(t, logger.entered, "first limiter wait")
			waitTrafficCancellationSignal(t, logger.entered, "second limiter wait")
			closed := make(chan struct{})
			go func() {
				switch closeSide {
				case "stream":
					stream.Close()
				case "client":
					c.Close()
				case "server":
					s.Close()
				}
				close(closed)
			}()
			waitTrafficCancellationSignal(t, logger.exited, "first limiter cancellation")
			waitTrafficCancellationSignal(t, logger.exited, "second limiter cancellation")
			waitTrafficCancellationSignal(t, closed, "transport shutdown")
			if closeSide == "stream" {
				logger.wait.Store(false)
				if err := echoOnce(t, c, startTCPEcho(t), []byte("same connection still usable")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRealQUICCloseCancelsDatagramPacing(t *testing.T) {
	logger := &cancellableTrafficLogger{entered: make(chan struct{}, 2), exited: make(chan struct{}, 2), rescue: make(chan struct{})}
	logger.wait.Store(true)
	defer close(logger.rescue)
	addr := startVerdictServer(t, &server.Config{TrafficLogger: logger})
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echo := &udpEchoServer{Conn: packet}
	go echo.Serve()
	defer echo.Close()
	c, _, err := client.NewClient(&client.Config{ServerAddr: addr, TLSConfig: client.TLSConfig{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.UDP()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Send([]byte("paced response"), packet.LocalAddr().String()); err != nil {
		t.Fatal(err)
	}
	waitTrafficCancellationSignal(t, logger.entered, "post-send pacing")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	waitTrafficCancellationSignal(t, logger.exited, "post-send pacing cancellation")
}
