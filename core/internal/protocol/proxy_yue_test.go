package protocol

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"testing"

	protocolerrors "github.com/apernet/hysteria/core/v2/errors"
	"github.com/apernet/quic-go/quicvarint"
)

func TestParseUDPMessageDirectDecoderMatchesWireFormat(t *testing.T) {
	for i := range 10_000 {
		addrLen := 1 + rand.IntN(200)
		dataLen := 1 + rand.IntN(1400)
		want := UDPMessage{
			SessionID: rand.Uint32(),
			PacketID:  uint16(rand.Uint32()),
			FragID:    uint8(rand.Uint32()),
			FragCount: uint8(rand.Uint32()),
			Addr:      string(bytes.Repeat([]byte{byte(i)}, addrLen)),
			Data:      bytes.Repeat([]byte{byte(i >> 8)}, dataLen),
		}
		wire := make([]byte, want.Size())
		if n := want.Serialize(wire); n != len(wire) {
			t.Fatalf("Serialize() = %d, want %d", n, len(wire))
		}
		got, err := ParseUDPMessage(wire)
		if err != nil {
			t.Fatalf("iteration %d: ParseUDPMessage() error = %v", i, err)
		}
		if got.SessionID != want.SessionID || got.PacketID != want.PacketID ||
			got.FragID != want.FragID || got.FragCount != want.FragCount ||
			got.Addr != want.Addr || !bytes.Equal(got.Data, want.Data) {
			t.Fatalf("iteration %d: decoded message mismatch\ngot  %#v\nwant %#v", i, got, want)
		}
	}
}

func TestParseUDPMessageMatchesLegacyAcceptance(t *testing.T) {
	for i := range 100_000 {
		wire := make([]byte, rand.IntN(256))
		for j := range wire {
			wire[j] = byte(rand.Uint32())
		}
		got, gotErr := ParseUDPMessage(wire)
		want, wantErr := parseUDPMessageLegacy(wire)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("iteration %d length %d: new err=%v legacy err=%v wire=%x", i, len(wire), gotErr, wantErr, wire)
		}
		if gotErr == nil && (got.SessionID != want.SessionID || got.PacketID != want.PacketID ||
			got.FragID != want.FragID || got.FragCount != want.FragCount ||
			got.Addr != want.Addr || !bytes.Equal(got.Data, want.Data)) {
			t.Fatalf("iteration %d: new=%#v legacy=%#v", i, got, want)
		}
	}
}

func TestParseUDPMessageRejectsMalformedFrames(t *testing.T) {
	valid := (&UDPMessage{SessionID: 1, PacketID: 2, FragCount: 1, Addr: "a", Data: []byte{1}})
	wire := make([]byte, valid.Size())
	valid.Serialize(wire)
	for n := range len(wire) {
		if _, err := ParseUDPMessage(wire[:n]); err == nil {
			t.Fatalf("accepted truncated frame of length %d", n)
		}
	}

	zeroAddr := make([]byte, 10)
	binary.BigEndian.PutUint32(zeroAddr, 1)
	zeroAddr[8] = 0
	zeroAddr[9] = 1
	if _, err := ParseUDPMessage(zeroAddr); err == nil {
		t.Fatal("accepted zero-length address")
	}

	tooLong := append(make([]byte, 8), quicvarint.Append(nil, MaxMessageLength+1)...)
	tooLong = append(tooLong, 1)
	if _, err := ParseUDPMessage(tooLong); err == nil {
		t.Fatal("accepted oversized address")
	}
}

func BenchmarkParseUDPMessage(b *testing.B) {
	want := UDPMessage{
		SessionID: 42,
		PacketID:  7,
		FragCount: 1,
		Addr:      "dns.example:53",
		Data:      bytes.Repeat([]byte{0x5a}, 1200),
	}
	wire := make([]byte, want.Size())
	want.Serialize(wire)
	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	for b.Loop() {
		if _, err := ParseUDPMessage(wire); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseUDPMessageLegacy(b *testing.B) {
	want := UDPMessage{
		SessionID: 42,
		PacketID:  7,
		FragCount: 1,
		Addr:      "dns.example:53",
		Data:      bytes.Repeat([]byte{0x5a}, 1200),
	}
	wire := make([]byte, want.Size())
	want.Serialize(wire)
	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	for b.Loop() {
		if _, err := parseUDPMessageLegacy(wire); err != nil {
			b.Fatal(err)
		}
	}
}

// parseUDPMessageLegacy is the exact pre-optimization decoder. Keeping it in
// tests makes acceptance parity and benchmark claims reproducible without
// retaining the reflection-heavy implementation in production.
func parseUDPMessageLegacy(msg []byte) (*UDPMessage, error) {
	m := &UDPMessage{}
	buf := bytes.NewBuffer(msg)
	if err := binary.Read(buf, binary.BigEndian, &m.SessionID); err != nil {
		return nil, err
	}
	if err := binary.Read(buf, binary.BigEndian, &m.PacketID); err != nil {
		return nil, err
	}
	if err := binary.Read(buf, binary.BigEndian, &m.FragID); err != nil {
		return nil, err
	}
	if err := binary.Read(buf, binary.BigEndian, &m.FragCount); err != nil {
		return nil, err
	}
	lAddr, err := quicvarint.Read(buf)
	if err != nil {
		return nil, err
	}
	if lAddr == 0 || lAddr > MaxMessageLength {
		return nil, protocolerrors.ProtocolError{Message: "invalid address length"}
	}
	bs := buf.Bytes()
	if len(bs) <= int(lAddr) {
		return nil, protocolerrors.ProtocolError{Message: "invalid message length"}
	}
	m.Addr = string(bs[:lAddr])
	m.Data = bs[lAddr:]
	return m, nil
}
