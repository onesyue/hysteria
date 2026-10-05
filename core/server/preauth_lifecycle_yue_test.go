package server

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
	"github.com/stretchr/testify/require"

	"github.com/apernet/hysteria/core/v2/internal/protocol"
)

type delayedPreAuthAuthenticator struct {
	entered chan struct{}
	resume  chan struct{}
}

func (a *delayedPreAuthAuthenticator) Authenticate(net.Addr, string, uint64) (bool, string) {
	close(a.entered)
	<-a.resume
	return true, "preauth-lifecycle-test"
}

type preAuthLifecycleLogger struct {
	meterLogger
	online chan struct{}
}

func (l *preAuthLifecycleLogger) LogOnlineState(_ string, online bool) {
	if online {
		close(l.online)
	}
}

// Exercise the actual HTTP/3 authentication handler and QUIC close callback.
// NotifyConnectionClosed runs before Conn.Context is canceled, so both sides
// of that cancellation boundary must release a late authentication's entry.
func TestPreAuthStateReleasedAfterDelayedAuthentication(t *testing.T) {
	for _, order := range []string{"authenticated_before_close", "during_close_notification", "after_context_cancellation"} {
		t.Run(order, func(t *testing.T) {
			auth := &delayedPreAuthAuthenticator{entered: make(chan struct{}), resume: make(chan struct{})}
			resumeAuth := sync.OnceFunc(func() { close(auth.resume) })
			logger := &preAuthLifecycleLogger{online: make(chan struct{})}
			notified := make(chan *quic.Conn, 1)
			resumeClosed := make(chan struct{})
			finishClosed := sync.OnceFunc(func() { close(resumeClosed) })
			if order != "during_close_notification" {
				finishClosed()
			}
			cert, err := tls.LoadX509KeyPair("../internal/integration_tests/test.crt", "../internal/integration_tests/test.key")
			require.NoError(t, err)
			udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
			require.NoError(t, err)
			srv, err := NewServer(&Config{
				Conn:          udp,
				TLSConfig:     TLSConfig{Certificates: []tls.Certificate{cert}},
				Authenticator: auth,
				TrafficLogger: logger,
				DisableUDP:    true,
				QUICConfig: QUICConfig{
					PreAuthReceiveLimit: 64 << 10,
					NotifyConnectionClosed: func(conn *quic.Conn) {
						notified <- conn
						<-resumeClosed
					},
				},
			})
			require.NoError(t, err)
			s := srv.(*serverImpl)
			t.Cleanup(func() {
				resumeAuth()
				finishClosed()
				_ = s.Close()
			})
			go s.Serve()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := quic.DialAddr(ctx, udp.LocalAddr().String(), &tls.Config{
				InsecureSkipVerify: true,
				NextProtos:         []string{http3.NextProtoH3},
			}, &quic.Config{EnableDatagrams: true})
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.CloseWithError(0, "") })
			transport := &http3.Transport{EnableDatagrams: true}
			httpClient := transport.NewClientConn(client)
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+protocol.URLHost+protocol.URLPath, nil)
			require.NoError(t, err)
			protocol.AuthRequestToHeader(request.Header, protocol.AuthRequest{Auth: "test"})
			response := make(chan int, 1)
			go func() {
				resp, err := httpClient.RoundTrip(request)
				if err != nil {
					response <- 0
					return
				}
				_ = resp.Body.Close()
				response <- resp.StatusCode
			}()
			expectTrafficSignal(t, auth.entered, "real authentication handler")
			if order == "authenticated_before_close" {
				resumeAuth()
				expectTrafficSignal(t, logger.online, "authentication publication")
				select {
				case status := <-response:
					require.Equal(t, protocol.StatusAuthOK, status)
				case <-ctx.Done():
					t.Fatal("authenticated client did not receive its response")
				}
			}
			require.NoError(t, client.CloseWithError(0, ""))
			var serverConn *quic.Conn
			select {
			case serverConn = <-notified:
			case <-ctx.Done():
				t.Fatal("QUIC close notification was not reached")
			}
			require.False(t, s.preAuth.tracked(serverConn), "QUIC close must first remove the existing entry")
			if order == "after_context_cancellation" {
				expectTrafficSignal(t, serverConn.Context().Done(), "connection context cancellation")
			} else if order == "during_close_notification" {
				require.NoError(t, serverConn.Context().Err(), "test must reach the pre-cancellation close window")
			}
			resumeAuth()
			expectTrafficSignal(t, logger.online, "late authentication publication")
			finishClosed()
			require.NoError(t, s.Close())
			require.False(t, s.preAuth.tracked(serverConn), "completed HTTP/3 handlers must not retain a closed QUIC connection")
		})
	}
}
