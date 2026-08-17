package congestion

// Regression coverage for the v2.11.0 small-MTU BBR panic fixed by upstream
// 26a03b08 (PR #1654, shipped in app/v2.12.0) and carried in this fork as
// 4aa5b1f8.
//
// Incident shape: UseBBR used to seed the replacement controller with
// bbr.GetInitialPacketSize(remoteAddr) - an address-family guess (1280 for
// UDP). With the Chrome parrot enabled by default since v2.11.0, or on any
// small-MTU path, the connection's actual initial packet size can be lower
// than that guess. quic-go later reports its real size to the controller via
// SetMaxDatagramSize; bbrSender cannot represent a size decrease and panics
// with "congestion BUG: decreased max datagram size". The fix seeds with
// min(conn.InitialPacketSize(), address guess), so the seed can never sit
// above what QUIC actually starts at.

import (
	"testing"

	"github.com/apernet/hysteria/core/v2/internal/congestion/bbr"
	"github.com/apernet/quic-go/congestion"
)

const (
	// quic-go protocol.InitialPacketSize: what GetInitialPacketSize guesses
	// for a *net.UDPAddr peer.
	addrGuessPacketSize congestion.ByteCount = 1280
	// A connection whose real initial packet size is below the address
	// guess - the Chrome-parrot / small-MTU case that triggered the panic.
	smallInitialPacketSize congestion.ByteCount = 1200
)

func TestSeedPacketSizeClampsToQUICInitial(t *testing.T) {
	tests := []struct {
		name             string
		quicSize, byAddr congestion.ByteCount
		want             congestion.ByteCount
	}{
		{"small MTU parrot conn clamps below address guess", smallInitialPacketSize, addrGuessPacketSize, smallInitialPacketSize},
		{"quic size unknown falls back to address guess", 0, addrGuessPacketSize, addrGuessPacketSize},
		{"negative quic size falls back to address guess", -1, addrGuessPacketSize, addrGuessPacketSize},
		{"address guess stays a floor when quic starts higher", 1452, addrGuessPacketSize, addrGuessPacketSize},
		{"equal sizes pass through", addrGuessPacketSize, addrGuessPacketSize, addrGuessPacketSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := seedPacketSize(tt.quicSize, tt.byAddr); got != tt.want {
				t.Fatalf("seedPacketSize(%d, %d) = %d, want %d", tt.quicSize, tt.byAddr, got, tt.want)
			}
		})
	}
}

// TestBBRSeededAboveQUICInitialPanics pins the invariant seedPacketSize
// exists to protect: a bbrSender seeded above the connection's real initial
// packet size panics as soon as quic-go reports that real size. This is the
// pre-fix code path (seed = address guess, unclamped). If this test ever
// stops panicking, the controller has learned to shrink and the clamp - and
// this pin - can be reconsidered.
func TestBBRSeededAboveQUICInitialPanics(t *testing.T) {
	sender := bbr.NewBbrSender(bbr.DefaultClock{}, addrGuessPacketSize, bbr.ProfileStandard)
	defer func() {
		if recover() == nil {
			t.Fatal("SetMaxDatagramSize below the seeded size did not panic; the seed clamp in UseBBR may no longer be required")
		}
	}()
	sender.SetMaxDatagramSize(smallInitialPacketSize)
}

// TestBBRSeededWithClampSurvivesSmallMTUConn walks the fixed seeding through
// the sequence that used to crash the server: a connection whose initial
// packet size is below the address guess, followed by quic-go reporting the
// real size and then a successful upward MTU probe.
func TestBBRSeededWithClampSurvivesSmallMTUConn(t *testing.T) {
	seed := seedPacketSize(smallInitialPacketSize, addrGuessPacketSize)
	sender := bbr.NewBbrSender(bbr.DefaultClock{}, seed, bbr.ProfileStandard)
	sender.SetMaxDatagramSize(smallInitialPacketSize) // quic-go reports the real initial size
	sender.SetMaxDatagramSize(addrGuessPacketSize)    // MTU probe up
	sender.SetMaxDatagramSize(1452)                   // further probe up
}
