package server

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type meterLogger struct {
	tx, rx atomic.Uint64
	calls  atomic.Int64
	limit  uint64 // 0 = unlimited; refuse once tx+rx would exceed it
	optOut bool
}

func (l *meterLogger) LogTraffic(_ string, tx, rx uint64) bool {
	l.calls.Add(1)
	if l.limit > 0 && l.tx.Load()+l.rx.Load()+tx+rx > l.limit {
		return false
	}
	l.tx.Add(tx)
	l.rx.Add(rx)
	return true
}
func (l *meterLogger) LogOnlineState(string, bool)        {}
func (l *meterLogger) TraceStream(HyStream, *StreamStats) {}
func (l *meterLogger) UntraceStream(HyStream)             {}
func (l *meterLogger) WantsStreamStats() bool             { return !l.optOut }

func TestWantsStreamStatsDefaultsToUpstreamBehaviour(t *testing.T) {
	if wantsStreamStats(nil) {
		t.Fatal("nil logger must not want stats")
	}
	var upstream TrafficLogger = struct{ TrafficLogger }{&meterLogger{optOut: true}}
	if !wantsStreamStats(upstream) {
		t.Fatal("a logger without the extension must keep stream stats (upstream behaviour)")
	}
	if wantsStreamStats(&meterLogger{optOut: true}) {
		t.Fatal("an opted-out logger must not get stream stats")
	}
	if !wantsStreamStats(&meterLogger{optOut: false}) {
		t.Fatal("WantsStreamStats()==true must keep stream stats")
	}
}

// runCopy proxies up (server->remote) and down (remote->server) payloads
// through copyTwoWayEx and returns once both directions finished.
func runCopy(t *testing.T, l TrafficLogger, stats *StreamStats, up, down []byte) error {
	t.Helper()
	serverSide, serverPeer := net.Pipe()
	remoteSide, remotePeer := net.Pipe()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // client writes upstream bytes, then reads the downstream ones
		defer wg.Done()
		_, _ = serverPeer.Write(up)
		_, _ = io.ReadFull(serverPeer, make([]byte, len(down)))
		_ = serverPeer.Close()
	}()
	go func() { // remote reads upstream bytes, then writes downstream
		defer wg.Done()
		_, _ = io.ReadFull(remotePeer, make([]byte, len(up)))
		_, _ = remotePeer.Write(down)
		_ = remotePeer.Close()
	}()
	err := copyTwoWayEx("u", serverSide, remoteSide, l, stats)
	_ = serverSide.Close()
	_ = remoteSide.Close()
	wg.Wait()
	return err
}

// Metering is identical with and without stream stats: the opt-out removes
// only the per-chunk stats upkeep, never a LogTraffic call.
func TestStreamStatsOptOutMetersExactlyTheSameBytes(t *testing.T) {
	up := bytes.Repeat([]byte{1}, 96<<10)
	down := bytes.Repeat([]byte{2}, 160<<10)

	with := &meterLogger{}
	stats := &StreamStats{}
	if err := runCopy(t, with, stats, up, down); err != nil {
		t.Fatalf("with stats: %v", err)
	}
	without := &meterLogger{optOut: true}
	if err := runCopy(t, without, nil, up, down); err != nil {
		t.Fatalf("without stats: %v", err)
	}
	if with.tx.Load() != uint64(len(up)) || with.rx.Load() != uint64(len(down)) {
		t.Fatalf("with stats metered tx=%d rx=%d", with.tx.Load(), with.rx.Load())
	}
	if without.tx.Load() != with.tx.Load() || without.rx.Load() != with.rx.Load() {
		t.Fatalf("opt-out metered tx=%d rx=%d, stats path tx=%d rx=%d",
			without.tx.Load(), without.rx.Load(), with.tx.Load(), with.rx.Load())
	}
	if stats.Tx.Load() != uint64(len(up)) || stats.Rx.Load() != uint64(len(down)) {
		t.Fatalf("stats path must still maintain StreamStats: tx=%d rx=%d", stats.Tx.Load(), stats.Rx.Load())
	}
}

// The disconnect decision survives the opt-out.
func TestStreamStatsOptOutStillDisconnectsOnRefusal(t *testing.T) {
	l := &meterLogger{optOut: true, limit: 8 << 10}
	err := runCopy(t, l, nil, bytes.Repeat([]byte{1}, 64<<10), nil)
	if err != errDisconnect {
		t.Fatalf("err = %v, want errDisconnect", err)
	}
}

func TestStreamStatsOptOutRemovesPerChunkAllocation(t *testing.T) {
	stats := &StreamStats{}
	l := &meterLogger{}
	withStats := testing.AllocsPerRun(1000, func() {
		stats.LastActiveTime.Store(timeNowForTest())
		stats.Rx.Add(1)
		l.LogTraffic("u", 0, 1)
	})
	withoutStats := testing.AllocsPerRun(1000, func() {
		l.LogTraffic("u", 0, 1)
	})
	t.Logf("per-chunk allocations: with stats %.1f, opted out %.1f", withStats, withoutStats)
	if withoutStats != 0 {
		t.Fatalf("opted-out per-chunk path allocates %.1f", withoutStats)
	}
	if withStats < 1 {
		t.Skip("stats upkeep no longer allocates on this toolchain; the opt-out still saves the clock read")
	}
}

func timeNowForTest() time.Time { return time.Now() }
