package cmd

import (
	"net"
	"runtime"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/core/v2/server"
)

// Darwin can allocate a dual-stack wildcard socket a port already owned by an
// IPv4 socket, then deliver every IPv4 reply to the other listener (Go #67226).
// This fixture uses IPv4 loopback destinations exclusively. Keep Linux and
// other platforms on the production default sockets, and bind only the Darwin
// fixture to its actual address family. The QUIC/TLS/ECH paths are unchanged.
// https://github.com/golang/go/issues/67226#issuecomment-5983315048
func configureECHTestClient(c *client.Config) {
	if runtime.GOOS == "darwin" {
		c.ConnFactory = echLoopbackConnFactory{}
	}
}

func configureECHTestServer(c *server.Config) {
	if runtime.GOOS == "darwin" {
		// NewServer has filled Outbound; wrap it before starting Serve so TCP
		// and outbound policy retain their production implementation.
		c.Outbound = &echLoopbackOutbound{Outbound: c.Outbound}
	}
}

type echLoopbackConnFactory struct{}

func (echLoopbackConnFactory) New(net.Addr) (net.PacketConn, error) {
	return net.ListenPacket("udp4", "127.0.0.1:0")
}

type echLoopbackOutbound struct{ server.Outbound }

func (*echLoopbackOutbound) UDP(string) (server.UDPConn, error) {
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return &echLoopbackUDPConn{PacketConn: c}, nil
}

type echLoopbackUDPConn struct{ net.PacketConn }

func (c *echLoopbackUDPConn) ReadFrom(b []byte) (int, string, error) {
	n, addr, err := c.PacketConn.ReadFrom(b)
	if addr == nil {
		return n, "", err
	}
	return n, addr.String(), err
}

func (c *echLoopbackUDPConn) WriteTo(b []byte, addr string) (int, error) {
	target, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return 0, err
	}
	return c.PacketConn.WriteTo(b, target)
}
