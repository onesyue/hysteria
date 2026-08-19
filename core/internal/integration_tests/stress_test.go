package integration_tests

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"golang.org/x/time/rate"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/core/v2/internal/integration_tests/mocks"
	"github.com/apernet/hysteria/core/v2/server"
)

type tcpStressor struct {
	DialFunc   func() (net.Conn, error)
	Size       int
	Parallel   int
	Iterations int
}

func (s *tcpStressor) Run(t *testing.T) {
	// Make some random data
	sData := make([]byte, s.Size)
	_, err := rand.Read(sData)
	assert.NoError(t, err)

	// Run iterations
	for i := 0; i < s.Iterations; i++ {
		var wg sync.WaitGroup
		errChan := make(chan error, s.Parallel)
		for j := 0; j < s.Parallel; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				conn, err := s.DialFunc()
				if err != nil {
					errChan <- err
					return
				}
				defer conn.Close()
				go conn.Write(sData)

				rData := make([]byte, len(sData))
				_, err = io.ReadFull(conn, rData)
				if err != nil {
					errChan <- err
					return
				}
			}()
		}
		wg.Wait()
		close(errChan)
		for err := range errChan {
			assert.NoError(t, err)
		}
	}
}

type udpStressor struct {
	ListenFunc func() (client.HyUDPConn, error)
	ServerAddr string
	Size       int
	Count      int
	Parallel   int
	Iterations int
}

func (s *udpStressor) Run(t *testing.T) {
	// Make some random data
	sData := make([]byte, s.Size)
	_, err := rand.Read(sData)
	assert.NoError(t, err)

	// Due to UDP's unreliability, limit sending to 1 MiB/s. Keep the burst
	// substantially below the QUIC / kernel queues: a 1 MiB burst lets the
	// entire 1000x100-byte case bypass rate limiting and makes packet loss
	// depend on resource pressure from earlier tests in the same process.
	limiter := rate.NewLimiter(1048576, 16*1024)

	// Run iterations
	for i := 0; i < s.Iterations; i++ {
		var wg sync.WaitGroup
		errChan := make(chan error, s.Parallel)
		for j := 0; j < s.Parallel; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				conn, err := s.ListenFunc()
				if err != nil {
					errChan <- err
					return
				}
				defer conn.Close()

				// All workers share the limiter, so budget for this iteration's total
				// payload plus a generous processing margin. Receive has no deadline;
				// without this bound, one extra dropped datagram can hang the whole suite.
				expected := time.Duration(int64(s.Size)*int64(s.Count)*int64(s.Parallel)) * time.Second / 1048576
				timeout := max(10*time.Second, 4*expected+5*time.Second)
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()

				var sent atomic.Int64
				var received atomic.Int64
				sendDone := make(chan error, 1)
				receiveDone := make(chan error, 1)
				go func() {
					// Sending routine
					for i := 0; i < s.Count; i++ {
						if err := limiter.WaitN(ctx, len(sData)); err != nil {
							sendDone <- fmt.Errorf("rate limit after %d/%d packets: %w", sent.Load(), s.Count, err)
							return
						}
						if err := conn.Send(sData, s.ServerAddr); err != nil {
							sendDone <- fmt.Errorf("send after %d/%d packets: %w", sent.Load(), s.Count, err)
							return
						}
						sent.Add(1)
					}
					sendDone <- nil
				}()

				minCount := s.Count * 8 / 10 // Tolerate 20% packet loss
				go func() {
					for i := 0; i < minCount; i++ {
						rData, _, err := conn.Receive()
						if err != nil {
							receiveDone <- err
							return
						}
						if len(rData) != len(sData) {
							receiveDone <- fmt.Errorf("incomplete data received: %d/%d bytes", len(rData), len(sData))
							return
						}
						received.Add(1)
					}
					receiveDone <- nil
				}()

				for sendDone != nil || receiveDone != nil {
					select {
					case err := <-sendDone:
						sendDone = nil
						if err != nil {
							errChan <- err
							return
						}
					case err := <-receiveDone:
						receiveDone = nil
						if err != nil {
							errChan <- err
							return
						}
					case <-ctx.Done():
						errChan <- fmt.Errorf(
							"UDP stress timed out after %s: sent %d/%d, received %d/%d: %w",
							timeout, sent.Load(), s.Count, received.Load(), minCount, ctx.Err(),
						)
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errChan)
		for err := range errChan {
			assert.NoError(t, err)
		}
	}
}

func TestClientServerTCPStress(t *testing.T) {
	// Create server
	udpConn, udpAddr, err := serverConn()
	assert.NoError(t, err)
	auth := mocks.NewMockAuthenticator(t)
	auth.EXPECT().Authenticate(mock.Anything, mock.Anything, mock.Anything).Return(true, "nobody")
	s, err := server.NewServer(&server.Config{
		TLSConfig:     serverTLSConfig(),
		Conn:          udpConn,
		Authenticator: auth,
	})
	assert.NoError(t, err)
	defer s.Close()
	go s.Serve()

	// Create TCP echo server
	echoAddr := "127.0.0.1:22333"
	echoListener, err := net.Listen("tcp", echoAddr)
	assert.NoError(t, err)
	echoServer := &tcpEchoServer{Listener: echoListener}
	defer echoServer.Close()
	go echoServer.Serve()

	// Create client
	c, _, err := client.NewClient(&client.Config{
		ServerAddr: udpAddr,
		TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
	})
	assert.NoError(t, err)
	defer c.Close()

	dialFunc := func() (net.Conn, error) {
		return c.TCP(echoAddr)
	}

	t.Run("Single 500m", (&tcpStressor{DialFunc: dialFunc, Size: 524288000, Parallel: 1, Iterations: 1}).Run)

	t.Run("Sequential 1000x1m", (&tcpStressor{DialFunc: dialFunc, Size: 1048576, Parallel: 1, Iterations: 1000}).Run)
	t.Run("Sequential 10000x100k", (&tcpStressor{DialFunc: dialFunc, Size: 102400, Parallel: 1, Iterations: 10000}).Run)

	t.Run("Parallel 100x10m", (&tcpStressor{DialFunc: dialFunc, Size: 10485760, Parallel: 100, Iterations: 1}).Run)
	t.Run("Parallel 1000x1m", (&tcpStressor{DialFunc: dialFunc, Size: 1048576, Parallel: 1000, Iterations: 1}).Run)
}

func TestClientServerUDPStress(t *testing.T) {
	// Create server
	udpConn, udpAddr, err := serverConn()
	assert.NoError(t, err)
	auth := mocks.NewMockAuthenticator(t)
	auth.EXPECT().Authenticate(mock.Anything, mock.Anything, mock.Anything).Return(true, "nobody")
	s, err := server.NewServer(&server.Config{
		TLSConfig:     serverTLSConfig(),
		Conn:          udpConn,
		Authenticator: auth,
	})
	assert.NoError(t, err)
	defer s.Close()
	go s.Serve()

	// Create UDP echo server
	echoAddr := "127.0.0.1:22333"
	echoConn, err := net.ListenPacket("udp", echoAddr)
	assert.NoError(t, err)
	echoServer := &udpEchoServer{Conn: echoConn}
	defer echoServer.Close()
	go echoServer.Serve()

	// Create client
	c, _, err := client.NewClient(&client.Config{
		ServerAddr: udpAddr,
		TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
	})
	assert.NoError(t, err)
	defer c.Close()

	t.Run("Single 1000x100b", (&udpStressor{
		ListenFunc: c.UDP,
		ServerAddr: echoAddr,
		Size:       100,
		Count:      1000,
		Parallel:   1,
		Iterations: 1,
	}).Run)
	t.Run("Single 1000x3k", (&udpStressor{
		ListenFunc: c.UDP,
		ServerAddr: echoAddr,
		Size:       3000,
		Count:      1000,
		Parallel:   1,
		Iterations: 1,
	}).Run)

	t.Run("5 Sequential 1000x100b", (&udpStressor{
		ListenFunc: c.UDP,
		ServerAddr: echoAddr,
		Size:       100,
		Count:      1000,
		Parallel:   1,
		Iterations: 5,
	}).Run)
	t.Run("5 Sequential 200x3k", (&udpStressor{
		ListenFunc: c.UDP,
		ServerAddr: echoAddr,
		Size:       3000,
		Count:      200,
		Parallel:   1,
		Iterations: 5,
	}).Run)

	t.Run("2 Sequential 5 Parallel 1000x100b", (&udpStressor{
		ListenFunc: c.UDP,
		ServerAddr: echoAddr,
		Size:       100,
		Count:      1000,
		Parallel:   5,
		Iterations: 2,
	}).Run)
	t.Run("2 Sequential 5 Parallel 200x3k", (&udpStressor{
		ListenFunc: c.UDP,
		ServerAddr: echoAddr,
		Size:       3000,
		Count:      200,
		Parallel:   5,
		Iterations: 2,
	}).Run)
}
