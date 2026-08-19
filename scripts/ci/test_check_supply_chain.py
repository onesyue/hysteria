from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from scripts.ci.check_supply_chain import (
    QUIC_GO_MODULES,
    QUIC_GO_VERSION,
    quic_source_contract_errors,
)


class QuicSourceContractTests(unittest.TestCase):
    def make_contract(self, root: Path) -> None:
        (root / "go.work").write_text(
            "go 1.25.0\n\n"
            "use (\n\t./app\n\t./core\n\t./extras\n)\n\n"
            "replace github.com/apernet/quic-go => "
            f"github.com/onesyue/quic-go {QUIC_GO_VERSION}\n",
            encoding="utf-8",
        )
        (root / "go.work.sum").write_text(
            "golang.org/x/term v0.45.0/go.mod h1:test\n",
            encoding="utf-8",
        )
        for module in QUIC_GO_MODULES:
            module_dir = root / module
            module_dir.mkdir()
            (module_dir / "go.mod").write_text(
                f"module example.invalid/{module}\n\n"
                "replace github.com/apernet/quic-go => "
                f"github.com/onesyue/quic-go {QUIC_GO_VERSION}\n",
                encoding="utf-8",
            )
            (module_dir / "go.sum").write_text(
                f"github.com/onesyue/quic-go {QUIC_GO_VERSION} h1:test\n"
                f"github.com/onesyue/quic-go {QUIC_GO_VERSION}/go.mod h1:test\n",
                encoding="utf-8",
            )

    def test_complete_contract_passes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.make_contract(root)
            self.assertEqual(quic_source_contract_errors(root), [])

    def test_deleted_workspace_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.make_contract(root)
            (root / "go.work").unlink()
            errors = quic_source_contract_errors(root)
            self.assertTrue(any("workspace source of truth is missing" in e for e in errors))

    def test_split_module_pin_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.make_contract(root)
            app_mod = root / "app" / "go.mod"
            app_mod.write_text(
                app_mod.read_text(encoding="utf-8").replace(
                    QUIC_GO_VERSION,
                    "v0.61.1-0.20260805064420-870c4d4ab48b",
                ),
                encoding="utf-8",
            )
            errors = quic_source_contract_errors(root)
            self.assertTrue(any("app/go.mod" in error for error in errors))

    def test_workspace_pin_drift_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.make_contract(root)
            workspace = root / "go.work"
            workspace.write_text(
                workspace.read_text(encoding="utf-8").replace(
                    QUIC_GO_VERSION,
                    "v0.61.1-yue.1",
                ),
                encoding="utf-8",
            )
            errors = quic_source_contract_errors(root)
            self.assertTrue(any("workspace pin" in error for error in errors))

    def test_missing_checksum_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.make_contract(root)
            (root / "extras" / "go.sum").write_text("", encoding="utf-8")
            errors = quic_source_contract_errors(root)
            self.assertTrue(any("extras/go.sum" in error for error in errors))


if __name__ == "__main__":
    unittest.main()
