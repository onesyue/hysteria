package server

import (
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/apernet/hysteria/core/v2/internal/protocol"
)

// The idle cleanup loop used to be started unconditionally for every
// authenticated HY2 connection, with a 1 s ticker, even when the connection
// never carried a single UDP datagram. These tests pin the on-demand contract:
// no session -> no loop; first session -> loop; last session expired -> loop
// exits; a later session restarts it and still expires; Run returning stops
// everything.

type idleTestIO struct {
	msgs chan *protocol.UDPMessage
}

func (io *idleTestIO) ReceiveMessage() (*protocol.UDPMessage, error) {
	m, ok := <-io.msgs
	if !ok {
		return nil, errors.New("closed")
	}
	return m, nil
}
func (io *idleTestIO) SendMessage([]byte, *protocol.UDPMessage) error { return nil }
func (io *idleTestIO) Hook([]byte, *string) error                     { return nil }
func (io *idleTestIO) UDP(string) (UDPConn, error)                    { return newIdleTestConn(), nil }
func (io *idleTestIO) CheckUDP(string) error                          { return nil }

type idleTestConn struct {
	once   sync.Once
	closed chan struct{}
}

func newIdleTestConn() *idleTestConn { return &idleTestConn{closed: make(chan struct{})} }

func (c *idleTestConn) ReadFrom([]byte) (int, string, error) {
	<-c.closed
	return 0, "", errors.New("closed")
}
func (c *idleTestConn) WriteTo(b []byte, _ string) (int, error) { return len(b), nil }
func (c *idleTestConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

type idleTestEvents struct {
	mu     sync.Mutex
	closed int
}

func (e *idleTestEvents) New(uint32, string) {}
func (e *idleTestEvents) Close(uint32, error) {
	e.mu.Lock()
	e.closed++
	e.mu.Unlock()
}
func (e *idleTestEvents) closedCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func udpMsg(id uint32) *protocol.UDPMessage {
	return &protocol.UDPMessage{SessionID: id, FragCount: 1, Addr: "example.com:53", Data: []byte("x")}
}

func TestUDPIdleCleanupRunsOnlyWhileSessionsExist(t *testing.T) {
	defer goleak.VerifyNone(t)

	io := &idleTestIO{msgs: make(chan *protocol.UDPMessage, 4)}
	events := &idleTestEvents{}
	sm := newUDPSessionManager(io, events, 300*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- sm.Run() }()

	// A connection that never carries UDP never starts the loop.
	time.Sleep(3 * idleCleanupInterval / 2)
	if sm.idleCleanupActive() {
		t.Fatal("idle cleanup loop running with zero sessions")
	}

	io.msgs <- udpMsg(1)
	waitFor(t, "first session to start the loop", 2*time.Second, sm.idleCleanupActive)

	// The session idles out, and with it the loop.
	waitFor(t, "session expiry", 5*time.Second, func() bool { return events.closedCount() == 1 })
	waitFor(t, "loop exit after last session", 5*time.Second, func() bool { return !sm.idleCleanupActive() })
	if n := sm.Count(); n != 0 {
		t.Fatalf("sessions left = %d", n)
	}

	// A later session restarts the loop and is still expired by it.
	io.msgs <- udpMsg(2)
	waitFor(t, "second session to restart the loop", 2*time.Second, sm.idleCleanupActive)
	waitFor(t, "second session expiry", 5*time.Second, func() bool { return events.closedCount() == 2 })
	waitFor(t, "loop exit again", 5*time.Second, func() bool { return !sm.idleCleanupActive() })

	// Run returning closes remaining sessions and stops the loop (goleak).
	io.msgs <- udpMsg(3)
	waitFor(t, "third session", 2*time.Second, sm.idleCleanupActive)
	close(io.msgs)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
	waitFor(t, "loop exit on Run return", 2*time.Second, func() bool { return !sm.idleCleanupActive() })
	if got := events.closedCount(); got != 3 {
		t.Fatalf("closed sessions = %d, want 3", got)
	}
}

// An active session keeps the loop alive: expiry must not be skipped just
// because the loop is now conditional.
func TestUDPIdleCleanupKeepsRunningForActiveSessions(t *testing.T) {
	defer goleak.VerifyNone(t)

	io := &idleTestIO{msgs: make(chan *protocol.UDPMessage, 4)}
	events := &idleTestEvents{}
	sm := newUDPSessionManager(io, events, 800*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- sm.Run() }()

	io.msgs <- udpMsg(7)
	waitFor(t, "loop start", 2*time.Second, sm.idleCleanupActive)
	for range 6 {
		time.Sleep(400 * time.Millisecond)
		io.msgs <- udpMsg(7) // refresh Last
		if !sm.idleCleanupActive() {
			t.Fatal("loop exited while a session was active")
		}
	}
	if events.closedCount() != 0 {
		t.Fatal("an active session was expired")
	}
	close(io.msgs)
	<-done
}
