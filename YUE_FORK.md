# Yue Hysteria fork ledger

## Baseline

- Canonical repository: `https://github.com/apernet/hysteria`
- Canonical release: `app/v2.12.3`, `core/v2.12.3`, `extras/v2.12.3`
- Exact base commit: `e1366b173ccf5706e1e4630fe8aa654a4b574085`
  (`feat: add ECH key generation command`), merged on 2026-09-22. Previous
  bases: `62d1016707af21b91e5fb6070311d9f016ff2754` (2026-09-14),
  `619a6f856b69fb7ee6a7a379e810e68b84004605`.

The Yue branch retains the upstream 2.12 Mimic fixes, Unix-domain masquerade
support, extra ACME providers, optional stateless-reset switch, and dual-stack
Mimic address filtering. Upstream's BBR initial-packet-size and stateless-reset
changes supersede the equivalent older Yue commits and were not duplicated.

## Retained Yue contracts

1. Authentication-to-connection registration is atomic, so credential removal
   can eagerly close the exact active connection without publishing a stale
   successful authentication.
2. The embedding service can enforce process and per-connection receive-memory
   budgets, and all acquired bytes are released on every shutdown path.
3. `DisableGSO` reaches both client and server QUIC transports.
4. BBR and Brutal are registered as factories, preserving the selected
   congestion controller after QUIC path migration and port hopping.
5. Downstream fragmented UDP traffic is charged exactly once and failed sends
   are not charged.
6. Graceful shutdown, mock lifecycle, build provenance, immutable action refs,
   private dependency checkout, and signed nested-tag contracts remain fail
   closed.
7. The geosite ACL matcher answers from a lazily built full/root index instead
   of a linear scan over the category; the attribute gate is folded into the
   index at build time, and the index is differential-tested against an
   independent transcription of upstream's scan
   (`extras/outbounds/acl/matchers_v2geo_index_yue_test.go`).
8. A traffic logger that implements `StreamStatsOptOut` and answers `false`
   is not handed `TraceStream`/`UntraceStream`, and the TCP copy loop skips the
   per-chunk `StreamStats` upkeep (a clock read plus a boxed `atomic.Value`
   store, one heap allocation per read and direction). `LogTraffic` still runs
   for every chunk, so metering, limits and disconnects are unchanged. Loggers
   without the extension keep upstream behaviour
   (`core/server/copy_stats_optout_yue_test.go`).
9. The UDP session manager's 1 s idle-cleanup loop runs only while the
   connection has at least one UDP session: it starts with the first session
   and exits after the last one expires, under the same lock that inserts and
   deletes sessions. A connection that never carries UDP owns no ticker
   (`core/server/udp_idle_cleanup_yue_test.go`).
10. A traffic logger that implements `TrafficVerdictLogger` separates
   "refuse this unit" from "disconnect the client". `TrafficReject` closes only
   the TCP stream whose chunk was refused, drops only the upstream datagram
   (the shared datagram receive loop never blocks on it), and is a no-op for a
   downstream datagram that was already sent; `TrafficDisconnect` keeps the
   old whole-connection close. Plain `LogTraffic` returning false still closes
   the connection, so loggers without the extension are unchanged
   (`core/internal/integration_tests/verdict_yue_test.go`,
   `core/server/udp_verdict_yue_test.go`).
11. `Config.AuthTimeout` closes a QUIC connection that has not authenticated
   within the timeout; the decision is taken under the auth mutex, so an
   authentication either finished first or can no longer finish. Zero (the
   default) keeps upstream behaviour.
12. `QUICConfig.PreAuthReceiveLimit` caps the received-but-unread bytes of one
   connection before it authenticates, in front of the embedder's own
   receive-memory callbacks; exceeding it refuses the receive and closes that
   connection. The cap stops applying at authentication. Zero disables it.
   Final connection cleanup joins HTTP/3 authentication handlers and QUIC's
   receive loop before forgetting this state again, so a slow authentication
   completing after the close notification cannot retain a closed connection.
13. Optional `ContextTrafficVerdictLogger` carries transport cancellation to
   blocking stream and downstream-datagram pacing callbacks. Stream closure,
   client/server shutdown and completion of either copy direction release the
   remaining waits. Sent datagrams remain chargeable; rejected unsent stream
   chunks do not become billable. The real QUIC cancellation regressions cover
   both stream directions and UDP (`traffic_cancel_yue_test.go`).
14. `Config.MaxUDPSessions` bounds each authenticated connection's UDP session
   map, including incomplete fragments, before hooks, socket creation or receive
   goroutines. Zero selects 256, negative values are invalid, and the standalone
   server exposes `maxUDPSessions`. New IDs at capacity are dropped; established
   sessions and other streams keep working. Idle expiry, failed dialing and
   connection shutdown release ownership exactly once. The default permits 256
   simultaneous UDP session IDs while bounding the receive-loop
   payload buffers to 2 MiB (two 4096-byte buffers per complete session), plus
   socket, goroutine, fragment and cache overhead. This is a per-connection
   boundary, not a claim that process-wide memory or multiple connections are
   bounded by 2 MiB. It is independent of QUIC stream/window settings. Admission
   is checked before receive accounting, so capacity-refused datagrams neither
   bill traffic nor consume rate tokens. Accepted receive/sent traffic callbacks
   are unchanged. Real local sockets, concurrent
   close/expiry and authenticated QUIC regressions cover admission and cleanup
   (`udp_session_limit_yue_test.go` in server and integration tests).

15. Optional `AuthenticatedOutbound` (2026-10-05): when the configured
   `Outbound` also implements `TCPAuthenticated(authID, reqAddr)`, each TCP
   request is dialled through it with the authenticated client's ID, so an
   embedder can make per-user dial decisions (yue-node gives each user a
   stable IPv6 source address of their own). Outbounds without the extension
   keep upstream's `TCP(reqAddr)`; UDP is unchanged. A real QUIC round trip
   covers both shapes (`core/internal/integration_tests/authenticated_outbound_yue_test.go`).
   Ships in `v2.12.3-yue.6`.

16. Optional `AuthenticatedUDPOutbound` (2026-10-06): when the configured
   `Outbound` also implements `UDPAuthenticated(authID, reqAddr)`, each UDP
   session socket is opened through it with the authenticated client's ID
   (reqAddr is the session's first destination; the socket still serves every
   destination of the session). yue-node wraps the returned `UDPConn` to
   attribute outgoing datagrams to a user for BitTorrent/DHT/uTP detection and
   per-user fan-out limits (fair-use endgame L4). Independent of the TCP
   extension; outbounds without it keep upstream's `UDP(reqAddr)`. A real QUIC
   round trip over two destinations covers both shapes
   (`core/internal/integration_tests/authenticated_udp_outbound_yue_test.go`).
   Not yet tagged: the next tag (`core/v2.12.3-yue.7`) carries it.

## Upstream sync 2026-09-14 (`core/v2.12.2-yue.3`, `extras/v2.12.2-yue.3`)

Merged upstream `master` through `62d1016` (five commits): the port hopping
redirect scope fix (wildcard listeners no longer redirect outbound UDP to
remote hosts, nftables and iptables), the rewritten TCP/UDP stress tests plus
their harness (re-enabled in CI, `-timeout=5m`, replacing the earlier Yue
bounding of the same tests), upstream's quic-go v0.62.0 bump (superseded by
the Yue pin below, module files kept from the fork), and the HTTP proxy plain
request body timeout fix.

The geosite index that had lived only in yue-node's vendor tree since
2026-09-08 now lives here. Measured on a 189,166-entry RootDomain category
(the shape of `category-ads-all`): miss path 2,194,462 ns/op linear vs
146 ns/op indexed, heap 22.0 MiB -> 12.7 MiB after the index releases the
per-entry slice.

## Native Initial datagram correction (2026-09-14)

Client and server start at the RFC 9000 1200-byte minimum, then retain native
DF and PMTUD to discover larger data datagrams. A real 1280-byte relay path
could not carry Chrome's 1250-byte Initial plus Salamander's 8-byte salt and
IPv4/UDP headers (1286 bytes). The server's larger default first flight also
failed. Allowing IP fragmentation in a diagnostic did not prove that the actual
DF-marked QUIC connection worked.

The pinned QUIC fork now respects a smaller explicitly requested Initial even
with Chrome parroting. The Chrome TLS/QUIC parameters, zero-length connection ID
and chaos protection stay enabled. Only the initial datagram budget changes.
See https://www.rfc-editor.org/rfc/rfc9000.html#section-14.3 .

The authenticated TCP echo regression uses real TLS, QUIC, Hysteria auth and
streams through a bidirectional size-limited path, including both IPv4 and IPv6
header budgets. The client-only candidate failed both paths with the old server;
the complete core race suite passes with both sides corrected.

## quic-go dependency order

All three modules, the workspace and CI checkout refs use signed
`github.com/onesyue/quic-go v0.62.0-yue.9` at
`d16e3afb4b22129eca62729369ad6f4ef396dd07`. This is the maintained private fork;
CI uses its existing authenticated source checkout. Remote tag verification and
authenticated native Go module download confirm the exact commit and checksums.
No dependency is fetched from an uncommitted local replacement in the release.

Matching `core/v2.12.3-yue.5` and `extras/v2.12.3-yue.5` tags identify this source
and dependency closure; prior immutable tags remain available.

yue.7 (2026-10-04) adds only the backport of canonical upstream #5876:
`quicvarint.Read`/`Peek` use data returned together with an error (for example
`io.EOF` on the final byte) instead of discarding it. Items 10-12 above ship in
the same `v2.12.3-yue.3` tags.

yue.9 (2026-10-05) includes yue.8, which restores the fork's cross-platform lint checks and removes
legacy build-constraint syntax without changing the QUIC wire behavior. It also
uses the existing race-build time scaling for QUIC deadline test setup and
teardown; the deadline behavior assertions are unchanged.
Item 13 above ships in `v2.12.3-yue.4`.
The final pre-auth cleanup in item 12 ships in `v2.12.3-yue.5`. Its real QUIC
regression covers authentication before close, during the close notification
(before context cancellation), and after context cancellation. The latter two
retain the connection on the preceding release and pass after final cleanup.

## Build toolchain baseline

CI and the container builder use Go `1.26.8`. The container builder is pinned
to the Docker Official Image `golang:1.26.8-alpine3.24` multi-platform OCI
index at
`sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628`.
The supply-chain guard binds both workflow pins and the Docker tag-plus-digest
to this baseline so a partial patch upgrade fails closed.

## Installer script publication ownership

The canonical upstream project owns the `hy2scripts` Cloudflare Pages project
and publishes the inherited `scripts/install_server.sh`. This Yue fork does not
own that Pages project and does not publish the `scripts/` directory. In
particular, changes to `scripts/ci/` are fork CI changes, not installer releases.

`.github/workflows/scripts.yml` therefore validates the inherited installer and
redirect contract without deployment permissions or Cloudflare credentials. If
publication ownership is deliberately transferred to this fork in the future,
provision a project-scoped Cloudflare Pages API token and account ID as GitHub
Actions secrets, then use Cloudflare's supported `wrangler-action`; do not
restore the archived `pages-action` or its Wrangler 2 runtime.

## Standalone crypto dependency floor (2026-10-05)

All three modules now select `golang.org/x/crypto v0.57.0`, matching the
version already selected by the embedding Node and Xray. `go work sync`
records its required transitive module versions and checksums consistently.
The previous v0.54.0 build did not import `golang.org/x/crypto/ssh`; the
September SSH advisories therefore do not establish a reachable Hysteria
vulnerability. This update removes the older standalone/probe dependency
floor while retaining the existing Go language version and QUIC fork.

## Complete dependency adoption (2026-09-22)

The embedding yue-node already overrode QUIC to yue.6, but the standalone
Hysteria modules, workspace and three CI source checkouts still consumed
yue.2. All eight projections now select yue.6, taking HTTP/3 request hardening,
read-batch ownership, congestion-size race fixes and bounded transient-read
backoff into standalone builds too. The v2.12.3 upstream merge adds only the
ECH key-generation tool and tests; the proxy-path fixes were already adopted
in the earlier merge. See https://github.com/HyNetworks/hysteria/releases/tag/app/v2.12.3 .

The optional SentTrafficLogger from 91c22e5 remains: admitted pre-send bytes
and successfully queued downstream datagrams have distinct callbacks, so a
limiter can avoid billing refused chunks without losing delivered datagrams.
