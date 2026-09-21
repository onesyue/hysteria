package server

import (
	"context"
	"errors"
	"testing"

	"github.com/apernet/hysteria/core/v2/internal/protocol"
	"github.com/apernet/quic-go"
)

// Downstream UDP was billed exactly twice for every datagram above the QUIC
// datagram limit, for as long as native HY2 has run.
//
// sendMessageAutoFrag sends a message whole first and, on
// quic.DatagramTooLargeError, re-sends it as fragments through the same
// udpIOImpl.SendMessage. That method charged the payload BEFORE handing it to
// the connection — so the attempt that could never succeed was charged in full,
// and then the fragments were charged again, summing to the same payload a
// second time. MaxDatagramFrameSize is 1200, so anything past ~1150 bytes is
// affected: video, games, DNS, QUIC-over-UDP.
//
// The guard asserts the invariant rather than the code shape: bytes charged ==
// bytes that actually reached the wire. A send that fails, and a message too
// large to serialize, must cost the user nothing.

type fakeUDPConn struct {
	maxDatagram int
	sent        [][]byte
	sendErr     error
	closed      bool
}

func (c *fakeUDPConn) SendDatagram(b []byte) error {
	if c.sendErr != nil {
		return c.sendErr
	}
	if c.maxDatagram > 0 && len(b) > c.maxDatagram {
		return &quic.DatagramTooLargeError{MaxDatagramPayloadSize: int64(c.maxDatagram)}
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	c.sent = append(c.sent, cp)
	return nil
}

func (c *fakeUDPConn) ReceiveDatagram(context.Context) ([]byte, error) {
	return nil, errors.New("not used")
}

func (c *fakeUDPConn) CloseWithError(quic.ApplicationErrorCode, string) error {
	c.closed = true
	return nil
}

type countingTrafficLogger struct {
	tx    uint64
	calls int
	allow bool
}

func (l *countingTrafficLogger) LogTraffic(_ string, _, tx uint64) bool {
	l.calls++
	l.tx += tx
	return l.allow
}

func (l *countingTrafficLogger) LogOnlineState(string, bool)        {}
func (l *countingTrafficLogger) TraceStream(HyStream, *StreamStats) {}
func (l *countingTrafficLogger) UntraceStream(HyStream)             {}

func newAccountingIO(conn *fakeUDPConn, logger *countingTrafficLogger) *udpIOImpl {
	return &udpIOImpl{Conn: conn, AuthID: "u", TrafficLogger: logger}
}

func msgOfSize(n int) *protocol.UDPMessage {
	return &protocol.UDPMessage{
		SessionID: 1,
		PacketID:  0,
		FragID:    0,
		FragCount: 1,
		Addr:      "192.0.2.1:53",
		Data:      make([]byte, n),
	}
}

func TestFragmentedDownstreamUDPIsBilledOnce(t *testing.T) {
	const payload = 1400
	conn := &fakeUDPConn{maxDatagram: 1200}
	logger := &countingTrafficLogger{allow: true}
	io := newAccountingIO(conn, logger)

	buf := make([]byte, 4096)
	if err := sendMessageAutoFrag(io, buf, msgOfSize(payload)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(conn.sent) < 2 {
		t.Fatalf("expected the message to be fragmented, got %d datagram(s)", len(conn.sent))
	}
	if logger.tx != payload {
		t.Fatalf("billed %d bytes for a %d-byte payload (%.2fx) — the oversized "+
			"attempt is being charged on top of the fragments", logger.tx, payload,
			float64(logger.tx)/float64(payload))
	}
}

func TestUnfragmentedDownstreamUDPIsStillBilledExactlyOnce(t *testing.T) {
	const payload = 512
	conn := &fakeUDPConn{maxDatagram: 1200}
	logger := &countingTrafficLogger{allow: true}
	io := newAccountingIO(conn, logger)

	if err := sendMessageAutoFrag(io, make([]byte, 4096), msgOfSize(payload)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if logger.calls != 1 || logger.tx != payload {
		t.Fatalf("calls=%d billed=%d, want 1 call / %d bytes", logger.calls, logger.tx, payload)
	}
}

func TestAFailedSendCostsTheUserNothing(t *testing.T) {
	conn := &fakeUDPConn{maxDatagram: 1200, sendErr: errors.New("connection gone")}
	logger := &countingTrafficLogger{allow: true}
	io := newAccountingIO(conn, logger)

	if err := io.SendMessage(make([]byte, 4096), msgOfSize(512)); err == nil {
		t.Fatal("expected the send error to propagate")
	}
	if logger.calls != 0 {
		t.Fatalf("charged %d time(s) for a datagram that never left the host", logger.calls)
	}
}

func TestAMessageTooLargeToSerializeIsNotBilled(t *testing.T) {
	conn := &fakeUDPConn{}
	logger := &countingTrafficLogger{allow: true}
	io := newAccountingIO(conn, logger)

	// A buffer far smaller than the message forces the silent-drop path.
	if err := io.SendMessage(make([]byte, 8), msgOfSize(4096)); err != nil {
		t.Fatalf("silent drop must not be an error: %v", err)
	}
	if logger.calls != 0 {
		t.Fatalf("charged %d time(s) for a message that was dropped before serialization", logger.calls)
	}
	if len(conn.sent) != 0 {
		t.Fatal("nothing should have been sent")
	}
}

func TestExceedingTheLimitStillDisconnects(t *testing.T) {
	conn := &fakeUDPConn{maxDatagram: 1200}
	logger := &countingTrafficLogger{allow: false}
	io := newAccountingIO(conn, logger)

	err := io.SendMessage(make([]byte, 4096), msgOfSize(512))
	if !errors.Is(err, errDisconnect) {
		t.Fatalf("expected errDisconnect, got %v", err)
	}
	if !conn.closed {
		t.Fatal("the connection must be closed when the traffic logger refuses")
	}
	// The datagram that carried the user past the limit is still delivered:
	// accounting now happens after the send, so at most one datagram overshoots.
	if len(conn.sent) != 1 {
		t.Fatalf("expected the in-flight datagram to have been sent, got %d", len(conn.sent))
	}
}

// sentAwareTrafficLogger implements SentTrafficLogger and records which
// callback each byte arrived through.
type sentAwareTrafficLogger struct {
	preSend  uint64
	postSend uint64
	allow    bool
}

func (l *sentAwareTrafficLogger) LogTraffic(_ string, tx, rx uint64) bool {
	l.preSend += tx + rx
	return l.allow
}

func (l *sentAwareTrafficLogger) LogSentTraffic(_ string, tx, rx uint64) bool {
	l.postSend += tx + rx
	return l.allow
}

func (l *sentAwareTrafficLogger) LogOnlineState(string, bool)        {}
func (l *sentAwareTrafficLogger) TraceStream(HyStream, *StreamStats) {}
func (l *sentAwareTrafficLogger) UntraceStream(HyStream)             {}

// A logger that can tell "already delivered" from "about to be forwarded" must
// be told: downstream UDP is logged after the send, so it arrives through
// LogSentTraffic, including on the refusal that disconnects the client.
func TestDownstreamUDPReachesSentTrafficLoggerAfterTheSend(t *testing.T) {
	for _, allow := range []bool{true, false} {
		conn := &fakeUDPConn{maxDatagram: 1200}
		logger := &sentAwareTrafficLogger{allow: allow}
		io := &udpIOImpl{Conn: conn, AuthID: "u", TrafficLogger: logger}

		err := sendMessageAutoFrag(io, make([]byte, 4096), msgOfSize(1400))
		if allow && err != nil {
			t.Fatalf("send: %v", err)
		}
		if !allow && !errors.Is(err, errDisconnect) {
			t.Fatalf("refused send error = %v, want errDisconnect", err)
		}
		if logger.preSend != 0 {
			t.Fatalf("allow=%v: %d delivered downstream bytes arrived through the pre-send LogTraffic", allow, logger.preSend)
		}
		if logger.postSend == 0 || len(conn.sent) == 0 {
			t.Fatalf("allow=%v: postSend=%d sent=%d, want the delivered datagram reported via LogSentTraffic", allow, logger.postSend, len(conn.sent))
		}
	}
}

// Upstream UDP is logged before it is forwarded, so it must stay on LogTraffic:
// a refusal there means the datagram is dropped.
func TestUpstreamUDPStaysOnPreSendLogTraffic(t *testing.T) {
	data := make([]byte, 64)
	msg := msgOfSize(100)
	n := msg.Serialize(data[:cap(data)])
	if n < 0 {
		data = make([]byte, 512)
		n = msg.Serialize(data)
	}
	conn := &receiveOnceUDPConn{datagram: data[:n]}
	logger := &sentAwareTrafficLogger{allow: false}
	io := &udpIOImpl{Conn: conn, AuthID: "u", TrafficLogger: logger}
	if _, err := io.ReceiveMessage(); !errors.Is(err, errDisconnect) {
		t.Fatalf("refused upstream receive error = %v, want errDisconnect", err)
	}
	if logger.preSend != 100 || logger.postSend != 0 {
		t.Fatalf("upstream bytes preSend=%d postSend=%d, want 100/0", logger.preSend, logger.postSend)
	}
}

type receiveOnceUDPConn struct {
	fakeUDPConn
	datagram []byte
}

func (c *receiveOnceUDPConn) ReceiveDatagram(context.Context) ([]byte, error) {
	if c.datagram == nil {
		return nil, errors.New("drained")
	}
	d := c.datagram
	c.datagram = nil
	return d, nil
}
