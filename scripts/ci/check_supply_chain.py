#!/usr/bin/env python3
"""Fail closed when fork CI or patched dependency pins drift."""

from __future__ import annotations

import re
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github" / "workflows"
ERRORS: list[str] = []


def require(condition: bool, message: str) -> None:
    if not condition:
        ERRORS.append(message)


test_workflow = (WORKFLOWS / "test.yml").read_text(encoding="utf-8")
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
    for line_number, line in enumerate(workflow.read_text(encoding="utf-8").splitlines(), 1):
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
    require('go-version: "1.26.6"' in contents, f"{workflow_name}: Go 1.26.6 pin missing")

for workflow_name in ("test.yml", "build-common.yml", "release.yml"):
    contents = (WORKFLOWS / workflow_name).read_text(encoding="utf-8")
    require('version: "0.12.0"' in contents, f"{workflow_name}: uv 0.12.0 pin missing")

dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")
for line_number, line in enumerate(dockerfile.splitlines(), 1):
    if line.startswith("FROM "):
        require(
            re.search(r"@sha256:[0-9a-f]{64}(?:\s|$)", line) is not None,
            f"Dockerfile:{line_number}: base image is not digest-pinned",
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
