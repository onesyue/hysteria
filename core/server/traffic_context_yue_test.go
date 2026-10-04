package server

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type waitingVerdictLogger struct {
	meterLogger
	entered chan struct{}
	exited  chan struct{}
	rescue  chan struct{}
	sent    atomic.Uint64
}

func newWaitingVerdictLogger(t *testing.T) *waitingVerdictLogger {
	t.Helper()
	l := &waitingVerdictLogger{entered: make(chan struct{}, 2), exited: make(chan struct{}, 2), rescue: make(chan struct{})}
	t.Cleanup(func() { close(l.rescue) })
	return l
}

func (l *waitingVerdictLogger) wait(ctx context.Context) TrafficVerdict {
	l.entered <- struct{}{}
	select {
	case <-ctx.Done():
	case <-l.rescue:
	}
	l.exited <- struct{}{}
	return TrafficReject
}

func (l *waitingVerdictLogger) LogStreamTraffic(string, uint64, uint64) TrafficVerdict {
	return l.wait(context.Background())
}

func (l *waitingVerdictLogger) LogDatagramTraffic(string, uint64, uint64) TrafficVerdict {
	return TrafficAccept
}

func (l *waitingVerdictLogger) LogSentDatagramTraffic(_ string, _, rx uint64) TrafficVerdict {
	l.sent.Add(rx)
	return l.wait(context.Background())
}

func (l *waitingVerdictLogger) LogStreamTrafficContext(ctx context.Context, _ string, _, _ uint64) TrafficVerdict {
	return l.wait(ctx)
}

func (l *waitingVerdictLogger) LogSentDatagramTrafficContext(ctx context.Context, _ string, _, rx uint64) TrafficVerdict {
	l.sent.Add(rx)
	return l.wait(ctx)
}

func expectTrafficSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestStreamTrafficWaitsCancelWithTransport(t *testing.T) {
	for _, statsEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "without_stats", true: "with_stats"}[statsEnabled], func(t *testing.T) {
			logger := newWaitingVerdictLogger(t)
			server, client := net.Pipe()
			remote, destination := net.Pipe()
			t.Cleanup(func() { server.Close(); client.Close(); remote.Close(); destination.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stats *StreamStats
			if statsEnabled {
				stats = &StreamStats{}
			}
			done := make(chan error, 1)
			go func() { done <- copyTwoWayEx(ctx, "user", server, remote, logger, stats) }()
			go client.Write([]byte("up"))
			go destination.Write([]byte("down"))
			expectTrafficSignal(t, logger.entered, "up/down wait")
			expectTrafficSignal(t, logger.entered, "second direction wait")
			cancel()
			expectTrafficSignal(t, logger.exited, "first cancellation")
			expectTrafficSignal(t, logger.exited, "second cancellation")
			select {
			case err := <-done:
				if err != errStreamRejected {
					t.Fatalf("got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("copy did not end after transport cancellation")
			}
		})
	}
}

func TestFinishedCopyCancelsOtherDirectionTrafficWait(t *testing.T) {
	logger := newWaitingVerdictLogger(t)
	server, client := net.Pipe()
	remote, destination := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close(); remote.Close(); destination.Close() })
	done := make(chan error, 1)
	go func() { done <- copyTwoWayEx(context.Background(), "user", server, remote, logger, nil) }()
	go client.Write([]byte("up"))
	expectTrafficSignal(t, logger.entered, "upstream wait")
	destination.Close()
	select {
	case err := <-done:
		if err != nil && err != io.EOF {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("copy did not observe peer close")
	}
	expectTrafficSignal(t, logger.exited, "orphaned upstream wait cancellation")
}

func TestSentDatagramWaitCancelsButKeepsDeliveredCharge(t *testing.T) {
	logger := newWaitingVerdictLogger(t)
	conn := &fakeUDPConn{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	udp := &udpIOImpl{Conn: conn, Context: ctx, AuthID: "user", TrafficLogger: logger}
	done := make(chan error, 1)
	go func() { done <- udp.SendMessage(make([]byte, 4096), msgOfSize(100)) }()
	expectTrafficSignal(t, logger.entered, "downstream datagram wait")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sent datagram failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("datagram pacing outlived connection")
	}
	if logger.sent.Load() != 100 {
		t.Fatalf("sent bytes charged %d times", logger.sent.Load())
	}
	if conn.closed {
		t.Fatal("cancelled pacing explicitly disconnected transport")
	}
}
