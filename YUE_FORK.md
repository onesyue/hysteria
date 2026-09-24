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
`github.com/onesyue/quic-go v0.62.0-yue.6` at
`2ea05b4b6cc0f7a9e6aabfdfd5fa4fbdae2c1078`. This is the maintained private fork;
CI uses its existing authenticated source checkout. Remote tag verification and
authenticated native Go module download confirm the exact commit and checksums.
No dependency is fetched from an uncommitted local replacement in the release.

Matching `core/v2.12.3-yue.1` and `extras/v2.12.3-yue.1` tags identify this source
and dependency closure; prior immutable tags remain available.

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
