package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreErrors "github.com/apernet/hysteria/core/v2/errors"
	"github.com/apernet/hysteria/core/v2/server"
	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestServerKeepAliveSession(t *testing.T) {
	const idleTimeout = 4 * time.Second
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	_, err := runCert(certOptions{
		Hosts: "localhost", CertFile: certFile, KeyFile: keyFile, ValidFor: time.Hour,
	})
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(readCertFile(t, certFile))

	newServer := func(t *testing.T, period string) (server.Server, string, error) {
		t.Helper()
		yaml := fmt.Sprintf(`listen: "127.0.0.1:0"
tls:
  cert: %q
  key: %q
auth:
  type: password
  password: loopback-test-only
quic:
  maxIdleTimeout: %s
`, certFile, keyFile, idleTimeout)
		if period != "" {
			yaml += "  keepAlivePeriod: " + period + "\n"
		}
		v := viper.New()
		v.SetConfigType("yaml")
		require.NoError(t, v.ReadConfig(strings.NewReader(yaml)))
		var config serverConfig
		require.NoError(t, v.Unmarshal(&config))
		sc, err := config.Config()
		require.NoError(t, err)
		t.Cleanup(func() { _ = sc.Conn.Close() })
		sc.EventLogger = nil // The command's global logger is initialized by the CLI.
		s, err := server.NewServer(sc)
		if err == nil {
			t.Cleanup(func() { _ = s.Close() })
		}
		return s, sc.Conn.LocalAddr().String(), err
	}

	for _, period := range []string{"-1s", "24h"} {
		t.Run(period, func(t *testing.T) {
			_, _, err := newServer(t, period)
			var invalid coreErrors.ConfigError
			require.ErrorAs(t, err, &invalid)
			require.Equal(t, "QUICConfig.KeepAlivePeriod", invalid.Field)
			require.Contains(t, invalid.Reason, "between 2s and 60s")
		})
	}

	for _, tc := range []struct{ name, period string }{
		{"configured", "2s"},
		{"omitted", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, addr, err := newServer(t, tc.period)
			require.NoError(t, err)
			go s.Serve()

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			// Hysteria's standard client enables its own keepalive. Dial QUIC
			// directly so only the server can keep the idle connection alive.
			conn, err := quic.DialAddr(ctx, addr, &tls.Config{
				ServerName: "localhost", RootCAs: roots, NextProtos: []string{http3.NextProtoH3},
			}, &quic.Config{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.CloseWithError(0, "") })
			h3 := (&http3.Transport{}).NewClientConn(conn)
			auth := func() {
				t.Helper()
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://hysteria/auth", nil)
				require.NoError(t, err)
				req.Header.Set("Hysteria-Auth", "loopback-test-only")
				resp, err := h3.RoundTrip(req)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				require.Equal(t, 233, resp.StatusCode)
			}
			auth()

			select {
			case <-conn.Context().Done():
				err := context.Cause(conn.Context())
				require.Empty(t, tc.period, "authenticated connection closed during idle interval: %v", err)
				var idle *quic.IdleTimeoutError
				require.ErrorAs(t, err, &idle)
			case <-time.After(idleTimeout + 2*time.Second):
				require.NotEmpty(t, tc.period, "connection stayed open with keepalive omitted")
				auth() // The same QUIC connection must still carry a round trip.
			}
		})
	}
}
