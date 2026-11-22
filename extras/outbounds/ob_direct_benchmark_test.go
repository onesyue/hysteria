package outbounds

import (
	"net"
	"testing"
)

// BenchmarkDirectUDPSmallPacket measures a complete 64-byte loopback delivery
// through the direct outbound with the same literal destination on each write.
func BenchmarkDirectUDPSmallPacket(b *testing.B) {
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatal(err)
	}
	defer peer.Close()
	dest := &AddrEx{Host: "127.0.0.1", Port: peer.LocalAddr().(*net.UDPAddr).AddrPort().Port()}
	conn, err := NewDirectOutboundSimple(DirectOutboundMode4).UDP(dest)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	var packet [64]byte
	b.ReportAllocs()
	b.SetBytes(int64(len(packet)))
	for b.Loop() {
		if _, err := conn.WriteTo(packet[:], dest); err != nil {
			b.Fatal(err)
		}
		if _, _, err := peer.ReadFromUDPAddrPort(packet[:]); err != nil {
			b.Fatal(err)
		}
	}
}
