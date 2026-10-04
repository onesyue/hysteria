package integration_tests

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apernet/quic-go"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/core/v2/server"
)

// verdictLogger is a hand-written TrafficVerdictLogger (yue fork). It rejects
// every unit whose payload length equals rejectLen and never disconnects.
// LogTraffic must not be reached once the extension is implemented.
type verdictLogger struct {
	rejectLen     uint64
	streamRejects atomic.Int32
	datagramDrops atomic.Int32
	legacyCalls   atomic.Int32
}

func (l *verdictLogger) verdict(tx, rx uint64) server.TrafficVerdict {
	if l.rejectLen != 0 && tx+rx == l.rejectLen {
		return server.TrafficReject
	}
	return server.TrafficAccept
}

func (l *verdictLogger) LogStreamTraffic(_ string, tx, rx uint64) server.TrafficVerdict {
	v := l.verdict(tx, rx)
	if v == server.TrafficReject {
		l.streamRejects.Add(1)
	}
	return v
}

func (l *verdictLogger) LogDatagramTraffic(_ string, tx, rx uint64) server.TrafficVerdict {
	v := l.verdict(tx, rx)
	if v == server.TrafficReject {
		l.datagramDrops.Add(1)
	}
	return v
}

func (l *verdictLogger) LogSentDatagramTraffic(string, uint64, uint64) server.TrafficVerdict {
	return server.TrafficReject // already sent: must never disconnect
}

func (l *verdictLogger) LogTraffic(string, uint64, uint64) bool {
	l.legacyCalls.Add(1)
	return false
}
func (l *verdictLogger) LogOnlineState(string, bool)                      {}
func (l *verdictLogger) TraceStream(server.HyStream, *server.StreamStats) {}
func (l *verdictLogger) UntraceStream(server.HyStream)                    {}

type acceptAll struct{}

func (acceptAll) Authenticate(net.Addr, string, uint64) (bool, string) { return true, "nobody" }

func startVerdictServer(t *testing.T, cfg *server.Config) net.Addr {
	t.Helper()
	udpConn, udpAddr, err := serverConn()
	if err != nil {
		t.Fatal(err)
	}
	cfg.TLSConfig = serverTLSConfig()
	cfg.Conn = udpConn
	if cfg.Authenticator == nil {
		cfg.Authenticator = acceptAll{}
	}
	s, err := server.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	t.Cleanup(func() { _ = s.Close() })
	return udpAddr
}

func startTCPEcho(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echo := &tcpEchoServer{Listener: l}
	go echo.Serve()
	t.Cleanup(func() { _ = echo.Close() })
	return l.Addr().String()
}

func echoOnce(t *testing.T, c client.Client, addr string, payload []byte) error {
	t.Helper()
	conn, err := c.TCP(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		return err
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch")
	}
	return nil
}

// A rejected stream chunk used to be a false from LogTraffic, which closed the
// whole QUIC connection: a rate limiter that declined one chunk disconnected
// every stream the user had. TrafficReject now ends only that stream.
func TestVerdictRejectClosesOnlyTheStream(t *testing.T) {
	logger := &verdictLogger{rejectLen: 13}
	addr := startVerdictServer(t, &server.Config{TrafficLogger: logger})
	echo := startTCPEcho(t)
	c, _, err := client.NewClient(&client.Config{ServerAddr: addr, TLSConfig: client.TLSConfig{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := echoOnce(t, c, echo, []byte("before")); err != nil {
		t.Fatalf("first stream: %v", err)
	}
	if err := echoOnce(t, c, echo, []byte("reject-me-now")); err == nil {
		t.Fatal("the rejected stream still echoed its chunk")
	}
	if logger.streamRejects.Load() == 0 {
		t.Fatal("the verdict logger was never asked about the rejected chunk")
	}
	// Same client, same QUIC connection: a later stream must still work.
	if err := echoOnce(t, c, echo, []byte("after the reject")); err != nil {
		t.Fatalf("a stream after one rejected stream failed (connection was closed): %v", err)
	}
	if n := logger.legacyCalls.Load(); n != 0 {
		t.Fatalf("LogTraffic called %d times for a verdict logger", n)
	}
}

// An upstream datagram the verdict logger rejects is dropped; the datagram
// loop goes on to the next one and the connection stays up.
func TestVerdictDatagramRejectDropsOnlyThatDatagram(t *testing.T) {
	logger := &verdictLogger{rejectLen: 4}
	addr := startVerdictServer(t, &server.Config{TrafficLogger: logger})
	echoConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echo := &udpEchoServer{Conn: echoConn}
	go echo.Serve()
	defer echo.Close()

	c, _, err := client.NewClient(&client.Config{ServerAddr: addr, TLSConfig: client.TLSConfig{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	uc, err := c.UDP()
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()

	if err := uc.Send([]byte("drop"), echoConn.LocalAddr().String()); err != nil {
		t.Fatal(err)
	}
	if err := uc.Send([]byte("keep it"), echoConn.LocalAddr().String()); err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 1)
	go func() {
		b, _, err := uc.Receive()
		if err == nil {
			got <- b
		}
	}()
	select {
	case b := <-got:
		if string(b) != "keep it" {
			t.Fatalf("received %q, want the accepted datagram only", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the datagram after a dropped one never arrived")
	}
	if logger.datagramDrops.Load() != 1 {
		t.Fatalf("datagram drops = %d, want 1", logger.datagramDrops.Load())
	}
	if err := echoOnce(t, c, startTCPEcho(t), []byte("still connected")); err != nil {
		t.Fatalf("connection did not survive a dropped datagram: %v", err)
	}
}

func dialRawQUIC(t *testing.T, addr net.Addr) *quic.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, addr.String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h3"}}, &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// An accepted QUIC connection that never authenticates used to live until the
// idle timeout, refreshed by anything it sent. AuthTimeout closes it.
func TestAuthTimeoutClosesUnauthenticatedConnection(t *testing.T) {
	addr := startVerdictServer(t, &server.Config{AuthTimeout: time.Second})
	conn := dialRawQUIC(t, addr)
	defer conn.CloseWithError(0, "")
	// Keep it busy so the idle timeout cannot be what closes it.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
				_ = conn.SendDatagram([]byte{0})
			}
		}
	}()
	select {
	case <-conn.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("an unauthenticated connection outlived AuthTimeout")
	}
}

// AuthTimeout must not touch a connection that authenticated in time.
func TestAuthTimeoutKeepsAuthenticatedConnection(t *testing.T) {
	addr := startVerdictServer(t, &server.Config{AuthTimeout: time.Second})
	echo := startTCPEcho(t)
	c, _, err := client.NewClient(&client.Config{ServerAddr: addr, TLSConfig: client.TLSConfig{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(1500 * time.Millisecond)
	if err := echoOnce(t, c, echo, []byte("past the auth deadline")); err != nil {
		t.Fatalf("authenticated connection broke after AuthTimeout: %v", err)
	}
}

func TestAuthTimeoutConfigValidation(t *testing.T) {
	udpConn, _, err := serverConn()
	if err != nil {
		t.Fatal(err)
	}
	defer udpConn.Close()
	_, err = server.NewServer(&server.Config{
		TLSConfig: serverTLSConfig(), Conn: udpConn, Authenticator: acceptAll{},
		AuthTimeout: 100 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("a sub-second AuthTimeout was accepted")
	}
}

// Before authentication a connection may hold only PreAuthReceiveLimit unread
// bytes; the flow-control window alone allowed megabytes per connection.
func TestPreAuthReceiveLimitClosesOversizedPreAuthSender(t *testing.T) {
	addr := startVerdictServer(t, &server.Config{QUICConfig: server.QUICConfig{PreAuthReceiveLimit: 16 << 10}})
	conn := dialRawQUIC(t, addr)
	defer conn.CloseWithError(0, "")
	stream, err := conn.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	// Not a valid HTTP/3 frame type the server acts on: the bytes sit unread.
	go func() { _, _ = stream.Write(bytes.Repeat([]byte{0x21}, 1<<20)) }()
	select {
	case <-conn.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a pre-auth sender exceeded PreAuthReceiveLimit without being closed")
	}
}

// The cap ends at authentication: an authenticated client moves far more than
// the pre-auth limit.
func TestPreAuthReceiveLimitDoesNotCapAuthenticatedTraffic(t *testing.T) {
	addr := startVerdictServer(t, &server.Config{QUICConfig: server.QUICConfig{PreAuthReceiveLimit: 16 << 10}})
	// The remote stalls before reading, so the server holds far more than the
	// pre-auth cap unread on an authenticated stream.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				time.Sleep(time.Second)
				_, _ = io.Copy(conn, conn)
				_ = conn.Close()
			}()
		}
	}()
	c, _, err := client.NewClient(&client.Config{ServerAddr: addr, TLSConfig: client.TLSConfig{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := echoOnce(t, c, l.Addr().String(), bytes.Repeat([]byte{7}, 512<<10)); err != nil {
		t.Fatalf("authenticated transfer beyond the pre-auth cap failed: %v", err)
	}
}
