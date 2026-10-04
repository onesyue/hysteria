package server

import (
	"errors"
	"testing"
)

type sentVerdictLogger struct {
	countingTrafficLogger
	verdict TrafficVerdict
	sent    uint64
}

func (l *sentVerdictLogger) LogStreamTraffic(string, uint64, uint64) TrafficVerdict {
	return TrafficAccept
}

func (l *sentVerdictLogger) LogDatagramTraffic(string, uint64, uint64) TrafficVerdict {
	return TrafficAccept
}

func (l *sentVerdictLogger) LogSentDatagramTraffic(_ string, _, rx uint64) TrafficVerdict {
	l.sent += rx
	return l.verdict
}

// A downstream datagram is already delivered when the verdict logger sees it:
// TrafficReject must neither close the connection nor fail the session send,
// and the delivered bytes are still reported once.
func TestSentDatagramRejectNeverDisconnects(t *testing.T) {
	conn := &fakeUDPConn{maxDatagram: 1200}
	logger := &sentVerdictLogger{verdict: TrafficReject}
	io := &udpIOImpl{Conn: conn, AuthID: "u", TrafficLogger: logger}
	if err := sendMessageAutoFrag(io, make([]byte, 4096), msgOfSize(1400)); err != nil {
		t.Fatalf("rejected sent datagram returned %v", err)
	}
	if conn.closed {
		t.Fatal("a rejected, already-sent datagram closed the connection")
	}
	if logger.sent != 1400 || logger.calls != 0 {
		t.Fatalf("reported sent=%d legacy calls=%d, want 1400 / 0", logger.sent, logger.calls)
	}
}

func TestSentDatagramDisconnectStillDisconnects(t *testing.T) {
	conn := &fakeUDPConn{}
	logger := &sentVerdictLogger{verdict: TrafficDisconnect}
	io := &udpIOImpl{Conn: conn, AuthID: "u", TrafficLogger: logger}
	if err := io.SendMessage(make([]byte, 4096), msgOfSize(10)); !errors.Is(err, errDisconnect) {
		t.Fatalf("disconnect verdict returned %v, want errDisconnect", err)
	}
	if !conn.closed {
		t.Fatal("disconnect verdict left the connection open")
	}
}
