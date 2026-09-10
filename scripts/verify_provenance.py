#!/usr/bin/env python3
"""Validate the embedded Codex source and native Marathon boundary."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path


HEX40 = re.compile(r"\b[0-9a-f]{40}\b")


def fail(message: str) -> None:
    raise RuntimeError(message)


def validate(root: Path) -> list[str]:
    provenance_path = root / "runtime" / "PROVENANCE.md"
    ledger_path = root / "runtime" / "PATCH_LEDGER.md"
    workspace_path = root / "runtime" / "codex-rs" / "Cargo.toml"
    native_manifest = root / "runtime" / "codex-rs" / "codexmarathon-runtime" / "Cargo.toml"
    adapter_manifest = root / "runtime" / "codexmarathon-adapter" / "Cargo.toml"
    for path in (provenance_path, ledger_path, workspace_path, native_manifest, adapter_manifest):
        if not path.is_file():
            fail(f"required provenance file is missing: {path.relative_to(root)}")

    provenance = provenance_path.read_text(encoding="utf-8")
    commits = HEX40.findall(provenance)
    if not commits:
        fail("runtime/PROVENANCE.md does not contain an upstream source commit")
    if "Apache-2.0" not in provenance:
        fail("runtime/PROVENANCE.md does not document the embedded license")
    for cargo in (root / "runtime" / "codex-rs").rglob("Cargo.toml"):
        text = cargo.read_text(encoding="utf-8", errors="replace")
        if re.search(r"(?:^|[\\/])donor(?:[\\/]|$)", text, re.IGNORECASE):
            fail(f"Cargo manifest references donor source: {cargo.relative_to(root)}")
    if (root / "runtime" / "codex-rs" / ".git").exists():
        fail("embedded workspace contains a nested .git directory")
    return [f"embedded Codex commit {commits[0]} and native Marathon manifests verified"]


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    args = parser.parse_args(argv)
    try:
        messages = validate(args.root.resolve())
    except (OSError, RuntimeError) as error:
        print(f"FAIL: provenance: {error}")
        return 1
    print("PASS: native provenance")
    for message in messages:
        print(f"INFO: {message}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
