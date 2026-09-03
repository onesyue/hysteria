#!/usr/bin/env python3
"""Fail closed when fork CI or patched dependency pins drift."""

from __future__ import annotations

import re
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github" / "workflows"
ERRORS: list[str] = []
QUIC_GO_VERSION = "v0.61.1-yue.5"
QUIC_GO_COMMIT = "0b16b6459525f62563847138875a07250141444d"
QUIC_GO_MODULES = ("app", "core", "extras")
GO_VERSION = "1.26.7"
GO_BUILDER_IMAGE = (
    "golang:1.26.7-alpine3.24@"
    "sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468"
)


def require(condition: bool, message: str) -> None:
    if not condition:
        ERRORS.append(message)


def quic_source_contract_errors(root: Path) -> list[str]:
    """Return source-level workspace and fork-pin drift errors.

    The checked-out commit used by CI is validated separately below.  Keeping
    this parser pure makes deletion and split-pin regressions testable without
    mutating the real checkout.
    """

    errors: list[str] = []
    workspace_path = root / "go.work"
    if not workspace_path.is_file():
        errors.append("go.work: workspace source of truth is missing")
    else:
        workspace = workspace_path.read_text(encoding="utf-8")
        workspace_uses = set(
            re.findall(r"(?m)^\s*(\./(?:app|core|extras))\s*$", workspace)
        )
        expected_uses = {f"./{module}" for module in QUIC_GO_MODULES}
        if workspace_uses != expected_uses:
            errors.append(
                "go.work: use block must contain exactly app, core, and extras"
            )
        expected_replace = (
            "replace github.com/apernet/quic-go => "
            f"github.com/onesyue/quic-go {QUIC_GO_VERSION}"
        )
        if re.search(rf"(?m)^{re.escape(expected_replace)}\s*$", workspace) is None:
            errors.append(
                f"go.work: quic-go workspace pin must be {QUIC_GO_VERSION}"
            )
    if not (root / "go.work.sum").is_file():
        errors.append("go.work.sum: workspace checksum manifest is missing")

    gitignore_path = root / ".gitignore"
    if gitignore_path.is_file() and re.search(
        r"(?m)^/?go\.work(?:\.sum)?/?$",
        gitignore_path.read_text(encoding="utf-8"),
    ):
        errors.append(".gitignore: committed workspace files must not be ignored")

    replace_re = re.compile(
        r"(?m)^replace\s+github\.com/apernet/quic-go\s+=>\s+"
        r"github\.com/onesyue/quic-go\s+(\S+)\s*$"
    )
    for module in QUIC_GO_MODULES:
        go_mod_path = root / module / "go.mod"
        if not go_mod_path.is_file():
            errors.append(f"{module}/go.mod: module manifest is missing")
            continue
        pins = replace_re.findall(go_mod_path.read_text(encoding="utf-8"))
        if pins != [QUIC_GO_VERSION]:
            rendered = ", ".join(pins) if pins else "missing"
            errors.append(
                f"{module}/go.mod: quic-go fork pin is {rendered}; "
                f"expected exactly {QUIC_GO_VERSION}"
            )

        go_sum_path = root / module / "go.sum"
        if not go_sum_path.is_file():
            errors.append(f"{module}/go.sum: checksum manifest is missing")
            continue
        go_sum = go_sum_path.read_text(encoding="utf-8")
        if f"github.com/onesyue/quic-go {QUIC_GO_VERSION} h1:" not in go_sum:
            errors.append(f"{module}/go.sum: {QUIC_GO_VERSION} checksum is missing")
        if f"github.com/onesyue/quic-go {QUIC_GO_VERSION}/go.mod h1:" not in go_sum:
            errors.append(
                f"{module}/go.sum: {QUIC_GO_VERSION} go.mod checksum is missing"
            )

    return errors


ERRORS.extend(quic_source_contract_errors(ROOT))


test_workflow = (WORKFLOWS / "test.yml").read_text(encoding="utf-8")
require(
    "python3 -m unittest scripts.ci.test_check_supply_chain" in test_workflow,
    "test.yml: supply-chain guard unit tests are not executed",
)
events_match = re.search(r"(?ms)^on:\n(?P<events>.*?)(?=^[^\s#])", test_workflow)
require(events_match is not None, "test.yml: missing top-level on block")
if events_match is not None:
    events = events_match.group("events")
    require(re.search(r"(?m)^  push:\s*$", events) is not None, "test.yml: push trigger missing")
    require(
        re.search(r"(?m)^  pull_request:\s*$", events) is not None,
        "test.yml: pull_request trigger missing",
    )
    require(
        re.search(r"(?m)^  workflow_dispatch:\s*$", events) is not None,
        "test.yml: workflow_dispatch trigger missing",
    )
    require("branches:" not in events, "test.yml: branch filters can exclude the default branch")

action_ref = re.compile(r"^\s*uses:\s*(?P<ref>[^\s#]+)")
immutable_action_ref = re.compile(r"^[^@]+@[0-9a-f]{40}$")
for workflow in sorted((*WORKFLOWS.glob("*.yml"), *WORKFLOWS.glob("*.yaml"))):
    workflow_contents = workflow.read_text(encoding="utf-8")
    require(
        "ACTIONS_ALLOW_UNSECURE_COMMANDS" not in workflow_contents,
        f"{workflow.relative_to(ROOT)}: legacy workflow command opt-out is forbidden",
    )
    require(
        "::set-output " not in workflow_contents
        and "::save-state " not in workflow_contents,
        f"{workflow.relative_to(ROOT)}: legacy stdout workflow command is forbidden",
    )
    for line_number, line in enumerate(workflow_contents.splitlines(), 1):
        match = action_ref.match(line)
        if match is None:
            continue
        ref = match.group("ref")
        if ref.startswith("./") or ref.startswith("docker://"):
            continue
        require(
            immutable_action_ref.fullmatch(ref) is not None,
            f"{workflow.relative_to(ROOT)}:{line_number}: mutable action ref {ref}",
        )

for workflow_name in ("test.yml", "build-common.yml"):
    contents = (WORKFLOWS / workflow_name).read_text(encoding="utf-8")
    require(
        f'go-version: "{GO_VERSION}"' in contents,
        f"{workflow_name}: Go {GO_VERSION} pin missing",
    )
    for dependency_path in ("core/go.sum", "extras/go.sum", "app/go.sum"):
        require(
            dependency_path in contents,
            f"{workflow_name}: Go cache dependency {dependency_path} missing",
        )

for workflow_name in ("test.yml", "build-common.yml", "release.yml"):
    contents = (WORKFLOWS / workflow_name).read_text(encoding="utf-8")
    require('version: "0.12.0"' in contents, f"{workflow_name}: uv 0.12.0 pin missing")

for workflow_name in ("test.yml", "build-common.yml", "docker.yml"):
    contents = (WORKFLOWS / workflow_name).read_text(encoding="utf-8")
    require("repository: onesyue/quic-go" in contents, f"{workflow_name}: private dependency checkout missing")
    require(f"ref: {QUIC_GO_COMMIT}" in contents, f"{workflow_name}: quic-go commit pin missing")
    require(
        "ssh-key: ${{ secrets.QUIC_GO_DEPLOY_KEY }}" in contents,
        f"{workflow_name}: read-only deploy key wiring missing",
    )
    require("persist-credentials: false" in contents, f"{workflow_name}: checkout credentials persist")

for workflow_name in ("test.yml", "build-common.yml"):
    contents = (WORKFLOWS / workflow_name).read_text(encoding="utf-8")
    require(
        "go work edit -replace=github.com/apernet/quic-go=./.ci/quic-go" in contents,
        f"{workflow_name}: local private dependency workspace missing",
    )

for workflow_name in ("release.yml", "master.yml", "experimental.yml"):
    contents = (WORKFLOWS / workflow_name).read_text(encoding="utf-8")
    require(
        "QUIC_GO_DEPLOY_KEY: ${{ secrets.QUIC_GO_DEPLOY_KEY }}" in contents,
        f"{workflow_name}: reusable build deploy key forwarding missing",
    )

dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")
require(
    f"FROM {GO_BUILDER_IMAGE} AS builder" in dockerfile,
    f"Dockerfile: Go {GO_VERSION} builder image or digest drifted",
)
for line_number, line in enumerate(dockerfile.splitlines(), 1):
    if line.startswith("FROM "):
        require(
            re.search(r"@sha256:[0-9a-f]{64}(?:\s|$)", line) is not None,
            f"Dockerfile:{line_number}: base image is not digest-pinned",
        )
require(
    "go work edit -replace=github.com/apernet/quic-go=./.ci/quic-go" in dockerfile,
    "Dockerfile: checked-out private dependency workspace missing",
)

hyperbole = (ROOT / "hyperbole.py").read_text(encoding="utf-8")
require(
    '["gofumpt", "-l", "-extra", *MODULE_SRC_DIRS]' in hyperbole,
    "hyperbole.py: format check must stay scoped to first-party modules",
)

uv_lock = (ROOT / "uv.lock").read_text(encoding="utf-8")
require(
    re.search(r'(?ms)^name = "cryptography"\nversion = "50\.0\.0"$', uv_lock) is not None,
    "uv.lock: cryptography 50.0.0 security pin missing",
)
for module in ("app", "extras"):
    go_mod = (ROOT / module / "go.mod").read_text(encoding="utf-8")
    require("github.com/pion/dtls/v3 v3.1.4" in go_mod, f"{module}/go.mod: dtls v3.1.4 pin missing")
    require("github.com/pion/stun/v3 v3.1.5" in go_mod, f"{module}/go.mod: stun v3.1.5 pin missing")

if ERRORS:
    print("supply-chain contract failed:", file=sys.stderr)
    for error in ERRORS:
        print(f"- {error}", file=sys.stderr)
    raise SystemExit(1)

print("supply-chain contract passed")
