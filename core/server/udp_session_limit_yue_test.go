package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/apernet/hysteria/core/v2/internal/protocol"
)

// Use real local UDP sockets: every admitted complete SessionID owns both a
// descriptor and the manager's receive goroutine, even for a one-byte packet.
type sessionLimitIO struct {
	idleTestIO
	mu       sync.Mutex
	conns    []*sessionLimitConn
	opened   atomic.Int32
	closed   atomic.Int32
	writes   atomic.Int32
	failDial atomic.Bool
}

type sessionLimitConn struct {
	*net.UDPConn
	owner *sessionLimitIO
	once  sync.Once
}

type sessionLimitAuth struct{}

func (sessionLimitAuth) Authenticate(net.Addr, string, uint64) (bool, string) { return true, "test" }

func (io *sessionLimitIO) UDP(string) (UDPConn, error) {
	if io.failDial.Swap(false) {
		return nil, errors.New("deliberate dial failure")
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	wrapped := &sessionLimitConn{UDPConn: c, owner: io}
	io.opened.Add(1)
	io.mu.Lock()
	io.conns = append(io.conns, wrapped)
	io.mu.Unlock()
	return wrapped, nil
}

func TestUDPSessionConfiguredLimit(t *testing.T) {
	for _, limit := range []int{-1, 0, 1, 512} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer pc.Close()
			cfg := Config{TLSConfig: TLSConfig{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }}, Conn: pc, Authenticator: sessionLimitAuth{}, MaxUDPSessions: limit}
			err = cfg.fill()
			if limit < 0 {
				if err == nil {
					t.Fatal("negative session cap accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := limit
			if want == 0 {
				want = 256
			}
			if cfg.MaxUDPSessions != want {
				t.Fatalf("cap=%d want %d", cfg.MaxUDPSessions, want)
			}
		})
	}
}

func TestUDPSessionLimitReturnsCapacityAfterIdleAndDialFailure(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	io := &sessionLimitIO{}
	sm := newUDPSessionManager(io, &idleTestEvents{}, time.Minute, 2)
	defer sm.cleanup(false)
	fragment := func(id uint32) *protocol.UDPMessage {
		return &protocol.UDPMessage{SessionID: id, PacketID: 1, FragCount: 2, Addr: "127.0.0.1:9", Data: []byte("x")}
	}
	sm.feed(fragment(1))
	sm.feed(fragment(2))
	sm.feed(fragment(3))
	if sm.Count() != 2 {
		t.Fatalf("sessions=%d", sm.Count())
	}
	sm.m[1].Last.Set(time.Now().Add(-2 * time.Minute))
	sm.cleanup(true)
	if sm.Count() != 1 {
		t.Fatalf("idle session not released: %d", sm.Count())
	}
	io.failDial.Store(true)
	sm.feed(&protocol.UDPMessage{SessionID: 3, FragCount: 1, Addr: "127.0.0.1:9", Data: []byte("x")})
	if sm.Count() != 1 {
		t.Fatalf("failed dial retained capacity: %d", sm.Count())
	}
	sm.feed(fragment(3))
	if sm.Count() != 2 || sm.m[2] == nil || sm.m[3] == nil {
		t.Fatal("released capacity not reusable, or old session evicted")
	}
}

func TestUDPSessionLimitRejectsBeforeAnyTrafficAccounting(t *testing.T) {
	for _, verdict := range []bool{false, true} {
		t.Run(fmt.Sprint(verdict), func(t *testing.T) {
			msg := msgOfSize(100)
			data := make([]byte, protocol.MaxUDPSize)
			data = data[:msg.Serialize(data)]
			conn := &receiveOnceUDPConn{datagram: data}
			base := &sentAwareTrafficLogger{allow: true}
			var logger TrafficLogger = base
			v := &sentVerdictLogger{countingTrafficLogger: countingTrafficLogger{allow: true}, verdict: TrafficAccept}
			if verdict {
				logger = v
			}
			sm := newUDPSessionManager(&idleTestIO{}, &idleTestEvents{}, time.Minute, 1)
			defer sm.cleanup(false)
			sm.feed(&protocol.UDPMessage{SessionID: 2, FragCount: 2, Addr: "127.0.0.1:9", Data: []byte("x")})
			io := &udpIOImpl{Conn: conn, AuthID: "u", TrafficLogger: logger, CanAdmitSession: sm.canAdmit}
			if got, err := io.ReceiveMessage(); got != nil || err == nil {
				t.Fatalf("capacity-refused datagram escaped: msg=%v err=%v", got, err)
			}
			if base.preSend != 0 || base.postSend != 0 || v.calls != 0 || v.sent != 0 {
				t.Fatalf("capacity refusal reached accounting: legacy=%+v verdict=%+v", base, v)
			}
			if conn.closed {
				t.Fatal("session cap closed QUIC connection")
			}
		})
	}
}

func TestUDPSessionLimitConcurrentExpiryAndSocketClose(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	target, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	io := &sessionLimitIO{}
	sm := newUDPSessionManager(io, &idleTestEvents{}, time.Millisecond, 8)
	defer sm.cleanup(false)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
				sm.cleanup(true)
				io.mu.Lock()
				if len(io.conns) > 0 {
					_ = io.conns[len(io.conns)-1].Close()
				}
				io.mu.Unlock()
			}
		}
	}()
	for id := uint32(1); id <= 2000; id++ {
		sm.feed(&protocol.UDPMessage{SessionID: id % 16, FragCount: 1, Addr: target.LocalAddr().String(), Data: []byte("x")})
		if got := sm.Count(); got > 8 {
			t.Errorf("cap exceeded: %d", got)
			break
		}
		if id%32 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	close(stop)
	<-done
	sm.cleanup(false)
	waitFor(t, "all session/socket ownership released", time.Second, func() bool { return sm.Count() == 0 && io.opened.Load() == io.closed.Load() })
}

func (c *sessionLimitConn) ReadFrom(b []byte) (int, string, error) {
	n, a, err := c.UDPConn.ReadFrom(b)
	if err != nil {
		return n, "", err
	}
	return n, a.String(), nil
}

func (c *sessionLimitConn) WriteTo(b []byte, addr string) (int, error) {
	a, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return 0, err
	}
	n, err := c.UDPConn.WriteToUDP(b, a)
	if err == nil {
		c.owner.writes.Add(1)
	}
	return n, err
}

func (c *sessionLimitConn) Close() error {
	c.once.Do(func() { _ = c.UDPConn.Close(); c.owner.closed.Add(1) })
	return nil
}

func TestUDPSessionDefaultLimitCapsIncompleteFragments(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	io := &sessionLimitIO{}
	sm := newUDPSessionManager(io, &idleTestEvents{}, time.Minute, defaultMaxUDPSessions)
	defer sm.cleanup(false)
	for id := uint32(1); id <= 257; id++ {
		sm.feed(&protocol.UDPMessage{SessionID: id, PacketID: 1, FragID: 0, FragCount: 2, Addr: "127.0.0.1:9", Data: []byte("x")})
	}
	if got := sm.Count(); got != 256 {
		t.Fatalf("incomplete fragments retained %d sessions, want cap 256", got)
	}
	if got := io.opened.Load(); got != 0 {
		t.Fatalf("incomplete fragments opened %d sockets", got)
	}
}

func TestUDPSessionDefaultLimitCapsRealSocketsAndReleases(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	target, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	io := &sessionLimitIO{}
	sm := newUDPSessionManager(io, &idleTestEvents{}, time.Minute, defaultMaxUDPSessions)
	defer sm.cleanup(false)
	for id := uint32(1); id <= 257; id++ {
		sm.feed(&protocol.UDPMessage{SessionID: id, FragCount: 1, Addr: target.LocalAddr().String(), Data: []byte("x")})
	}
	if got := sm.Count(); got != 256 {
		t.Fatalf("retained %d sessions, want cap 256", got)
	}
	if got := io.opened.Load(); got != 256 {
		t.Fatalf("opened %d sockets, want cap 256", got)
	}
	// Refusing a new ID must not evict or block an existing session.
	sm.feed(&protocol.UDPMessage{SessionID: 1, FragCount: 1, Addr: target.LocalAddr().String(), Data: []byte("still live")})
	if got := io.writes.Load(); got != 257 {
		t.Fatalf("writes=%d, existing session did not survive", got)
	}
	sm.cleanup(false)
	if got := sm.Count(); got != 0 {
		t.Fatalf("sessions after cleanup=%d", got)
	}
	if got := io.closed.Load(); got != 256 {
		t.Fatalf("closed sockets=%d", got)
	}
}
