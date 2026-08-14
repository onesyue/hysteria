package utils_test

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/apernet/hysteria/core/v2/client"
)

type MockEchoHyClient struct{}

func (c *MockEchoHyClient) TCP(addr string) (net.Conn, error) {
	return &mockEchoTCPConn{
		bufChan: make(chan []byte, 10),
		done:    make(chan struct{}),
	}, nil
}

func (c *MockEchoHyClient) UDP() (client.HyUDPConn, error) {
	return &mockEchoUDPConn{
		bufChan: make(chan mockEchoUDPPacket, 10),
		done:    make(chan struct{}),
	}, nil
}

func (c *MockEchoHyClient) Close() error {
	return nil
}

type mockEchoTCPConn struct {
	bufChan   chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func (c *mockEchoTCPConn) Read(b []byte) (n int, err error) {
	select {
	case <-c.done:
		return 0, io.EOF
	case buf := <-c.bufChan:
		return copy(b, buf), nil
	}
}

func (c *mockEchoTCPConn) Write(b []byte) (n int, err error) {
	// A synchronous Writer must not retain the caller's buffer after Write
	// returns. io.Copy immediately reuses its buffer for the next socket read;
	// queueing b itself makes the echo Read race that reuse.
	owned := append([]byte(nil), b...)
	select {
	case <-c.done:
		return 0, net.ErrClosed
	case c.bufChan <- owned:
		return len(b), nil
	}
}

func (c *mockEchoTCPConn) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}

func (c *mockEchoTCPConn) LocalAddr() net.Addr {
	// Not implemented
	return nil
}

func (c *mockEchoTCPConn) RemoteAddr() net.Addr {
	// Not implemented
	return nil
}

func (c *mockEchoTCPConn) SetDeadline(t time.Time) error {
	// Not implemented
	return nil
}

func (c *mockEchoTCPConn) SetReadDeadline(t time.Time) error {
	// Not implemented
	return nil
}

func (c *mockEchoTCPConn) SetWriteDeadline(t time.Time) error {
	// Not implemented
	return nil
}

type mockEchoUDPPacket struct {
	Data []byte
	Addr string
}

type mockEchoUDPConn struct {
	bufChan   chan mockEchoUDPPacket
	done      chan struct{}
	closeOnce sync.Once
}

func (c *mockEchoUDPConn) Receive() ([]byte, string, error) {
	select {
	case <-c.done:
		return nil, "", io.EOF
	case p := <-c.bufChan:
		return p.Data, p.Addr, nil
	}
}

func (c *mockEchoUDPConn) Send(b []byte, s string) error {
	// The production UDP implementation serializes the payload before Send
	// returns. Keep the mock's ownership identical: udpServer's datagram data
	// aliases its reusable receive buffer.
	p := mockEchoUDPPacket{
		Data: append([]byte(nil), b...),
		Addr: s,
	}
	select {
	case <-c.done:
		return net.ErrClosed
	case c.bufChan <- p:
		return nil
	}
}

func (c *mockEchoUDPConn) Close() error {
	// Close is allowed to race an in-flight Send/Receive. Signal closure on a
	// separate channel so both operations unblock without send-vs-close races.
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}
