# Yue Hysteria fork ledger

## Baseline

- Canonical repository: `https://github.com/apernet/hysteria`
- Canonical release: `app/v2.12.2`, `core/v2.12.2`, `extras/v2.12.2`
- Exact base commit: `619a6f856b69fb7ee6a7a379e810e68b84004605`
  (`feat: add quic.disableStatelessReset server option`)

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
`github.com/onesyue/quic-go v0.62.0-yue.2` at
`64f8901a4d53d1513003034dd7e1a9dbb0650a24`. This is the maintained private fork;
CI uses its existing authenticated source checkout. Remote tag verification and
authenticated native Go module download confirm the exact commit and checksums.
No dependency is fetched from an uncommitted local replacement in the release.

Matching `core/v2.12.2-yue.2` and `extras/v2.12.2-yue.2` tags identify this source
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
