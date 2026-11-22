package server

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
	"github.com/stretchr/testify/require"

	"github.com/apernet/hysteria/core/v2/errors"
)

func TestServerKeepAlivePeriod(t *testing.T) {
	cert, err := tls.LoadX509KeyPair("../internal/integration_tests/test.crt", "../internal/integration_tests/test.key")
	require.NoError(t, err)
	for _, tc := range []struct {
		period  time.Duration
		invalid bool
	}{
		{period: 0},
		{period: 2 * time.Second},
		{period: 60 * time.Second},
		{period: -time.Second, invalid: true},
		{period: 2*time.Second - time.Nanosecond, invalid: true},
		{period: 60*time.Second + time.Nanosecond, invalid: true},
	} {
		t.Run(tc.period.String(), func(t *testing.T) {
			udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = udp.Close() })
			srv, err := NewServer(&Config{
				Conn:          udp,
				TLSConfig:     TLSConfig{Certificates: []tls.Certificate{cert}},
				QUICConfig:    QUICConfig{KeepAlivePeriod: tc.period},
				Authenticator: sessionLimitAuth{},
			})
			if tc.invalid {
				var configErr errors.ConfigError
				require.ErrorAs(t, err, &configErr)
				require.Equal(t, "QUICConfig.KeepAlivePeriod", configErr.Field)
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = srv.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client, err := quic.DialAddr(ctx, udp.LocalAddr().String(), &tls.Config{
				InsecureSkipVerify: true,
				NextProtos:         []string{http3.NextProtoH3},
			}, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.CloseWithError(0, "") })
			conn, err := srv.(*serverImpl).listener.Accept(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.period, conn.Config().KeepAlivePeriod)
		})
	}
}
