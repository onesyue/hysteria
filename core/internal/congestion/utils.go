package congestion

import (
	"fmt"
	"strings"

	"github.com/apernet/hysteria/core/v2/internal/congestion/bbr"
	"github.com/apernet/hysteria/core/v2/internal/congestion/brutal"
	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/congestion"
)

const (
	TypeBBR  = "bbr"
	TypeReno = "reno"
)

func NormalizeType(congestionType string) (string, error) {
	switch normalized := strings.ToLower(congestionType); normalized {
	case "", TypeBBR:
		return TypeBBR, nil
	case TypeReno:
		return TypeReno, nil
	default:
		return "", fmt.Errorf("unsupported congestion type %q", congestionType)
	}
}

func NormalizeBBRProfile(profile string) (string, error) {
	normalized, err := bbr.ParseProfile(profile)
	if err != nil {
		return "", err
	}
	return string(normalized), nil
}

// UseBBR installs BBR as a factory, not as a single instance.
//
// The distinction is the difference between BBR running for the life of the
// connection and BBR running until the client's address first changes. This is
// called exactly once, from the authentication handler, and there is no second
// call to repair anything afterwards — so if a path migration were allowed to
// replace the controller, the connection would finish on whatever the QUIC
// stack substituted. With port hopping the client rebinds its socket every
// hop_interval, which is a migration every time, so "until the address first
// changes" means "for the first hop_interval and never again".
//
// Registering a factory lets the migration rebuild BBR in its initial state,
// which is what RFC 9000 section 9.4 actually asks for.
func UseBBR(conn *quic.Conn, profile bbr.Profile) {
	conn.SetCongestionControlFactory(func(initialPacketSize congestion.ByteCount) congestion.CongestionControl {
		// Re-read the remote address on every build: after a migration it is the
		// new path's, and it is what the address-based size guess is derived from.
		return bbr.NewBbrSender(
			bbr.DefaultClock{},
			seedPacketSize(initialPacketSize, bbr.GetInitialPacketSize(conn.RemoteAddr())),
			profile,
		)
	})
}

// seedPacketSize picks the datagram size to seed a replacement congestion
// controller with, given the size QUIC itself starts at and the guess derived
// from the remote address.
//
// The seed must not exceed what QUIC actually starts at. If it does, the first
// path MTU probe can land between the two: QUIC sees an increase and reports
// it, but the controller sees a decrease, which it cannot represent. Taking the
// smaller of the two keeps the address-based guess as a floor for connections
// whose path we can't reason about, while never seeding above QUIC.
func seedPacketSize(quicSize, byAddr congestion.ByteCount) congestion.ByteCount {
	if quicSize <= 0 {
		return byAddr
	}
	return min(quicSize, byAddr)
}

// UseBrutal installs Brutal as a factory for the same reason as UseBBR: a
// migration must not be able to quietly replace the operator's choice of
// congestion control with the QUIC stack's built-in sender.
func UseBrutal(conn *quic.Conn, tx uint64, disableLossCompensation bool) {
	conn.SetCongestionControlFactory(func(congestion.ByteCount) congestion.CongestionControl {
		// Brutal is a fixed-rate sender: it seeds its own datagram size and the
		// path's initial size tells it nothing it can use.
		return brutal.NewBrutalSender(tx, disableLossCompensation)
	})
}

func UseConfigured(conn *quic.Conn, congestionType, bbrProfile string) {
	switch congestionType {
	case TypeReno:
		return
	default:
		UseBBR(conn, bbr.Profile(bbrProfile))
	}
}
