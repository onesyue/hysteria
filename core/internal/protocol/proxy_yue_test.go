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
		want := UDPMessage{
			SessionID: rand.Uint32(),
			PacketID:  uint16(rand.Uint32()),
			FragID:    uint8(rand.Uint32()),
			FragCount: uint8(rand.Uint32()),
			Addr:      string(bytes.Repeat([]byte{byte(i)}, 1+rand.IntN(200))),
			Data:      bytes.Repeat([]byte{byte(i >> 8)}, 1+rand.IntN(1400)),
		}
		wire := make([]byte, want.Size())
		if n := want.Serialize(wire); n != len(wire) {
			t.Fatalf("Serialize() = %d, want %d", n, len(wire))
		}
		got, err := ParseUDPMessage(wire)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if got.SessionID != want.SessionID || got.PacketID != want.PacketID ||
			got.FragID != want.FragID || got.FragCount != want.FragCount ||
			got.Addr != want.Addr || !bytes.Equal(got.Data, want.Data) {
			t.Fatalf("iteration %d: decoded message mismatch", i)
		}
	}
}

func TestParseUDPMessageMatchesLegacyAcceptance(t *testing.T) {
	for i := range 10_000 {
		wire := make([]byte, rand.IntN(256))
		for j := range wire {
			wire[j] = byte(rand.Uint32())
		}
		got, gotErr := ParseUDPMessage(wire)
		want, wantErr := parseUDPMessageLegacy(wire)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("iteration %d length %d: direct err=%v legacy err=%v", i, len(wire), gotErr, wantErr)
		}
		if gotErr == nil && (got.SessionID != want.SessionID || got.PacketID != want.PacketID ||
			got.FragID != want.FragID || got.FragCount != want.FragCount ||
			got.Addr != want.Addr || !bytes.Equal(got.Data, want.Data)) {
			t.Fatalf("iteration %d: direct and legacy decoders differ", i)
		}
	}
}

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

func BenchmarkParseUDPMessageDirect(b *testing.B) {
	want := UDPMessage{SessionID: 42, PacketID: 7, FragCount: 1, Addr: "dns.example:53", Data: bytes.Repeat([]byte{0x5a}, 1200)}
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
	want := UDPMessage{SessionID: 42, PacketID: 7, FragCount: 1, Addr: "dns.example:53", Data: bytes.Repeat([]byte{0x5a}, 1200)}
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
