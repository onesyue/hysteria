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

## quic-go dependency order

The committed source is pinned to the signed
`github.com/onesyue/quic-go v0.62.0-yue.1` release at
`fc95ba2edd61fc8cfd9265d5f9c756f06d38919b`. The complete Hysteria 2.12.2
suite, including its TCP/UDP stress tests and the Yue receive/accounting tests,
was run against that exact fork tree.

Release in this order:

1. publish quic-go `v0.62.0-yue.1` at `fc95ba2e` (complete);
2. update all workspace/module pins, checksums, supply-chain constants, and CI
   checkout refs to that immutable release (complete);
3. rerun source, format, supply-chain, race, and release builds;
4. sign matching `core/v2.12.2-yue.1` and `extras/v2.12.2-yue.1` tags on the
   same Hysteria commit.

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
