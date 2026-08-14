package utils_test

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestMockEchoConnectionsOwnQueuedBuffers(t *testing.T) {
	tcp := &mockEchoTCPConn{
		bufChan: make(chan []byte, 1),
		done:    make(chan struct{}),
	}
	tcpInput := []byte("tcp-original")
	if _, err := tcp.Write(tcpInput); err != nil {
		t.Fatalf("TCP Write: %v", err)
	}
	clear(tcpInput)
	tcpOutput := make([]byte, len("tcp-original"))
	if _, err := tcp.Read(tcpOutput); err != nil {
		t.Fatalf("TCP Read: %v", err)
	}
	if got, want := string(tcpOutput), "tcp-original"; got != want {
		t.Fatalf("TCP echo = %q, want %q", got, want)
	}

	udp := &mockEchoUDPConn{
		bufChan: make(chan mockEchoUDPPacket, 1),
		done:    make(chan struct{}),
	}
	udpInput := []byte("udp-original")
	if err := udp.Send(udpInput, "example.com:443"); err != nil {
		t.Fatalf("UDP Send: %v", err)
	}
	clear(udpInput)
	udpOutput, addr, err := udp.Receive()
	if err != nil {
		t.Fatalf("UDP Receive: %v", err)
	}
	if got, want := string(udpOutput), "udp-original"; got != want {
		t.Fatalf("UDP echo = %q, want %q", got, want)
	}
	if addr != "example.com:443" {
		t.Fatalf("UDP addr = %q", addr)
	}
}

func TestMockEchoCloseIsConcurrentAndUnblocksIO(t *testing.T) {
	tcp := &mockEchoTCPConn{
		bufChan: make(chan []byte),
		done:    make(chan struct{}),
	}
	tcpReadDone := make(chan error, 1)
	go func() {
		_, err := tcp.Read(make([]byte, 1))
		tcpReadDone <- err
	}()
	if err := tcp.Close(); err != nil {
		t.Fatalf("TCP Close: %v", err)
	}
	if err := tcp.Close(); err != nil {
		t.Fatalf("second TCP Close: %v", err)
	}
	requireErrorWithin(t, tcpReadDone, io.EOF)

	udpSend := &mockEchoUDPConn{
		bufChan: make(chan mockEchoUDPPacket),
		done:    make(chan struct{}),
	}
	udpSendDone := make(chan error, 1)
	go func() {
		udpSendDone <- udpSend.Send([]byte("payload"), "example.com:443")
	}()
	if err := udpSend.Close(); err != nil {
		t.Fatalf("UDP Close during Send: %v", err)
	}
	if err := udpSend.Close(); err != nil {
		t.Fatalf("second UDP Close during Send: %v", err)
	}
	requireErrorWithin(t, udpSendDone, net.ErrClosed)

	udpReceive := &mockEchoUDPConn{
		bufChan: make(chan mockEchoUDPPacket),
		done:    make(chan struct{}),
	}
	udpReceiveDone := make(chan error, 1)
	go func() {
		_, _, err := udpReceive.Receive()
		udpReceiveDone <- err
	}()
	if err := udpReceive.Close(); err != nil {
		t.Fatalf("UDP Close during Receive: %v", err)
	}
	if err := udpReceive.Close(); err != nil {
		t.Fatalf("second UDP Close during Receive: %v", err)
	}
	requireErrorWithin(t, udpReceiveDone, io.EOF)
}

func requireErrorWithin(t *testing.T, result <-chan error, want error) {
	t.Helper()
	select {
	case err := <-result:
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("operation did not unblock after Close")
	}
}
