package integration_tests

import (
	"bytes"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/core/v2/server"
)

type limitedUDPOutbound struct{ opened, closed atomic.Int32 }

func (o *limitedUDPOutbound) TCP(addr string) (net.Conn, error) { return net.Dial("tcp", addr) }
func (o *limitedUDPOutbound) CheckUDP(string) error             { return nil }
func (o *limitedUDPOutbound) UDP(string) (server.UDPConn, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	o.opened.Add(1)
	return &limitedUDPConn{UDPConn: c, owner: o}, nil
}

type limitedUDPConn struct {
	*net.UDPConn
	owner *limitedUDPOutbound
	once  sync.Once
}

func (c *limitedUDPConn) ReadFrom(b []byte) (int, string, error) {
	n, a, e := c.UDPConn.ReadFrom(b)
	if e != nil {
		return n, "", e
	}
	return n, a.String(), nil
}

func (c *limitedUDPConn) WriteTo(b []byte, addr string) (int, error) {
	a, e := net.ResolveUDPAddr("udp4", addr)
	if e != nil {
		return 0, e
	}
	return c.UDPConn.WriteToUDP(b, a)
}

func (c *limitedUDPConn) Close() error {
	c.once.Do(func() { _ = c.UDPConn.Close(); c.owner.closed.Add(1) })
	return nil
}

type capTrafficLogger struct {
	verdictLogger
	sent, received atomic.Uint64
}

func (l *capTrafficLogger) LogDatagramTraffic(_ string, tx, rx uint64) server.TrafficVerdict {
	l.sent.Add(tx)
	return server.TrafficAccept
}

func (l *capTrafficLogger) LogSentDatagramTraffic(_ string, tx, rx uint64) server.TrafficVerdict {
	l.received.Add(rx)
	return server.TrafficAccept
}

func capWait(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func capEcho(t *testing.T, c client.HyUDPConn, addr string, payload []byte) {
	t.Helper()
	if err := c.Send(payload, addr); err != nil {
		t.Fatal(err)
	}
	type result struct {
		b []byte
		e error
	}
	done := make(chan result, 1)
	go func() { b, _, e := c.Receive(); done <- result{b, e} }()
	select {
	case r := <-done:
		if r.e != nil || !bytes.Equal(r.b, payload) {
			t.Fatalf("UDP echo=%q err=%v want=%q", r.b, r.e, payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("existing UDP session stopped delivering")
	}
}

// Real TLS/authenticated QUIC, real UDP descriptors, and the production config
// path. Refusing a third session must preserve both earlier sessions and TCP,
// keep the traffic callbacks, and close every admitted socket at connection end.
func TestUDPSessionCapKeepsAuthenticatedConnectionUsable(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	out := &limitedUDPOutbound{}
	meter := &capTrafficLogger{}
	addr := startVerdictServer(t, &server.Config{MaxUDPSessions: 2, Outbound: out, TrafficLogger: meter})
	echoConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echo := &udpEchoServer{Conn: echoConn}
	go echo.Serve()
	t.Cleanup(func() { _ = echo.Close() })
	c, _, err := client.NewClient(&client.Config{ServerAddr: addr, TLSConfig: client.TLSConfig{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	first, err := c.UDP()
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.UDP()
	if err != nil {
		t.Fatal(err)
	}
	capEcho(t, first, echoConn.LocalAddr().String(), []byte("one"))
	capEcho(t, second, echoConn.LocalAddr().String(), []byte("two"))
	third, err := c.UDP()
	if err != nil {
		t.Fatal(err)
	}
	if err = third.Send([]byte("refuse"), echoConn.LocalAddr().String()); err != nil {
		t.Fatal(err)
	}
	capEcho(t, first, echoConn.LocalAddr().String(), []byte("old one"))
	capEcho(t, second, echoConn.LocalAddr().String(), []byte("old two"))
	capWait(t, "accepted-only receive accounting", func() bool { return meter.sent.Load() == 20 })
	capWait(t, "exact sent-datagram accounting", func() bool { return meter.received.Load() == 20 })
	if n := out.opened.Load(); n != 2 {
		t.Fatalf("opened %d sockets, configured cap was 2", n)
	}
	if err = echoOnce(t, c, startTCPEcho(t), []byte("TCP also survives")); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	capWait(t, "all admitted UDP sockets to close", func() bool { return out.closed.Load() == 2 })
}
