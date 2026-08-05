package integration_tests

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/core/v2/server"
	"github.com/apernet/quic-go"
)

type staticAuthenticator struct{}

func (staticAuthenticator) Authenticate(net.Addr, string, uint64) (bool, string) {
	return true, "user"
}

type trackingLogger struct {
	mu       sync.Mutex
	accept   bool
	events   []string
	untracks int
}

var _ server.ConnectionTracker = (*trackingLogger)(nil)

func (l *trackingLogger) add(event string) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *trackingLogger) TrackConnection(string, func() error) (func(), bool) {
	l.add("track")
	if !l.accept {
		return nil, false
	}
	return sync.OnceFunc(func() {
		l.mu.Lock()
		l.untracks++
		l.events = append(l.events, "untrack")
		l.mu.Unlock()
	}), true
}

func (l *trackingLogger) LogTraffic(string, uint64, uint64) bool { return true }
func (l *trackingLogger) LogOnlineState(_ string, online bool) {
	if online {
		l.add("online")
	} else {
		l.add("offline")
	}
}
func (l *trackingLogger) TraceStream(server.HyStream, *server.StreamStats) {}
func (l *trackingLogger) UntraceStream(server.HyStream)                    {}

func (l *trackingLogger) snapshot() ([]string, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...), l.untracks
}

func waitForTrackerEvents(t *testing.T, l *trackingLogger, count int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, _ := l.snapshot()
		if len(events) >= count {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
	events, _ := l.snapshot()
	t.Fatalf("timed out waiting for %d tracker events; got %v", count, events)
	return nil
}

func TestConnectionTrackerRejectsStaleAuthentication(t *testing.T) {
	udpConn, udpAddr, err := serverConn()
	if err != nil {
		t.Fatal(err)
	}
	logger := &trackingLogger{accept: false}
	s, err := server.NewServer(&server.Config{
		TLSConfig:     serverTLSConfig(),
		Conn:          udpConn,
		Authenticator: staticAuthenticator{},
		TrafficLogger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go s.Serve()

	c, _, err := client.NewClient(&client.Config{
		ServerAddr: udpAddr,
		TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
	})
	if c != nil {
		_ = c.Close()
	}
	if err == nil {
		t.Fatal("stale authentication was published as success")
	}
	events := waitForTrackerEvents(t, logger, 1)
	if len(events) != 1 || events[0] != "track" {
		t.Fatalf("rejected tracker events = %v, want [track]", events)
	}
}

func TestConnectionTrackerCleansUpBeforeOffline(t *testing.T) {
	udpConn, udpAddr, err := serverConn()
	if err != nil {
		t.Fatal(err)
	}
	logger := &trackingLogger{accept: true}
	s, err := server.NewServer(&server.Config{
		TLSConfig:     serverTLSConfig(),
		Conn:          udpConn,
		Authenticator: staticAuthenticator{},
		TrafficLogger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go s.Serve()

	c, _, err := client.NewClient(&client.Config{
		ServerAddr: udpAddr,
		TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	events := waitForTrackerEvents(t, logger, 4)
	want := []string{"track", "online", "untrack", "offline"}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("tracker lifecycle = %v, want %v", events, want)
		}
	}
	_, untracks := logger.snapshot()
	if untracks != 1 {
		t.Fatalf("untrack calls = %d, want 1", untracks)
	}
}

func TestQUICConfigReceiveBudgetBridge(t *testing.T) {
	udpConn, udpAddr, err := serverConn()
	if err != nil {
		t.Fatal(err)
	}
	var allowed, released atomic.Uint64
	closed := make(chan struct{})
	var closeOnce sync.Once
	s, err := server.NewServer(&server.Config{
		TLSConfig:     serverTLSConfig(),
		Conn:          udpConn,
		Authenticator: staticAuthenticator{},
		QUICConfig: server.QUICConfig{
			AllowConnectionWindowIncrease: func(*quic.Conn, uint64) bool { return true },
			AllowConnectionReceive: func(_ *quic.Conn, delta uint64) bool {
				allowed.Add(delta)
				return true
			},
			ReleaseConnectionReceive: func(_ *quic.Conn, delta uint64) {
				released.Add(delta)
			},
			NotifyConnectionClosed: func(*quic.Conn) {
				closeOnce.Do(func() { close(closed) })
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go s.Serve()

	c, _, err := client.NewClient(&client.Config{
		ServerAddr: udpAddr,
		TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("NotifyConnectionClosed was not bridged")
	}
	if allowed.Load() == 0 {
		t.Fatal("AllowConnectionReceive was not bridged")
	}
	if released.Load() > allowed.Load() {
		t.Fatalf("released %d bytes after allowing %d", released.Load(), allowed.Load())
	}
}
