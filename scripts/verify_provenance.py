#!/usr/bin/env python3
"""Validate the source/provenance boundary before building a release.

This script intentionally uses only Python's standard library so it can run on
a clean CI image before Go or Rust are installed. It validates the tracked
embedded runtime, not the read-only donor checkouts, and never reads auth
files or credential state.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path


HEX40 = re.compile(r"\b[0-9a-f]{40}\b")


def fail(message: str) -> None:
    raise RuntimeError(message)


def relative_file(root: Path, value: str) -> Path:
    candidate = Path(value)
    if candidate.is_absolute() or ".." in candidate.parts:
        fail(f"manifest path is not repository-relative: {value!r}")
    path = root / candidate
    if not path.is_file():
        fail(f"required provenance file is missing: {value}")
    return path


def load_manifest(root: Path, manifest_arg: str | None) -> tuple[dict, Path]:
    manifest = Path(manifest_arg) if manifest_arg else root / "packaging" / "release-manifest.json"
    if not manifest.is_absolute():
        manifest = root / manifest
    if not manifest.is_file():
        fail(f"release manifest is missing: {manifest}")
    try:
        value = json.loads(manifest.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        fail(f"cannot read release manifest: {error}")
    if not isinstance(value, dict) or value.get("schema_version") != 1:
        fail("release manifest schema_version must be 1")
    return value, manifest


def validate(root: Path, manifest: dict, require_switch_clearance: bool) -> list[str]:
    messages: list[str] = []
    provenance = manifest.get("provenance")
    if not isinstance(provenance, dict):
        fail("release manifest has no provenance object")
    codext = provenance.get("codext")
    if not isinstance(codext, dict):
        fail("release manifest has no codext provenance")
    codext_commit = str(codext.get("commit", ""))
    if not HEX40.fullmatch(codext_commit):
        fail("codext provenance commit must be a 40-character hexadecimal SHA")
    if codext.get("license") != "Apache-2.0":
        fail("codext provenance must identify Apache-2.0")

    provenance_path = relative_file(root, "runtime/PROVENANCE.md")
    provenance_text = provenance_path.read_text(encoding="utf-8")
    if codext_commit not in provenance_text:
        fail("runtime/PROVENANCE.md does not contain the manifest codext commit")
    if "Apache-2.0" not in provenance_text:
        fail("runtime/PROVENANCE.md does not document the embedded license")
    for license_path in codext.get("license_files", []):
        relative_file(root, str(license_path))
    relative_file(root, "runtime/PATCH_LEDGER.md")

    cargo_manifests = list((root / "runtime" / "codex-rs").rglob("Cargo.toml"))
    if not cargo_manifests:
        fail("embedded runtime has no Cargo manifests")
    for cargo in cargo_manifests:
        text = cargo.read_text(encoding="utf-8", errors="replace")
        if re.search(r"(?:^|[\\/])donor(?:[\\/]|$)", text, re.IGNORECASE):
            fail(f"embedded Cargo manifest references donor source: {cargo.relative_to(root)}")
    if (root / "runtime" / "codex-rs" / ".git").exists():
        fail("embedded runtime contains a nested .git directory")

    for include in manifest.get("include", []):
        relative_file(root, str(include))

    switch = provenance.get("codex_switch")
    if not isinstance(switch, dict):
        fail("release manifest has no codex_switch provenance")
    if switch.get("distributed_source") is not False:
        fail("codex-switch source must remain excluded from release archives until licensed")
    if require_switch_clearance:
        clearance = str(switch.get("clearance", ""))
        if "required" in clearance.lower():
            fail("codex-switch redistribution clearance is still required")
    else:
        messages.append("codex-switch source is correctly excluded; redistribution clearance remains documented")

    # Scan only product files for obvious accidental inclusion of a donor
    # dependency. We intentionally do not scan donor/.
    scanned_roots = [
        root / "controller",
        root / "integration",
        root / "protocol",
        root / "runtime" / "codexmarathon-adapter",
        root / "runtime" / "codex-rs" / "codexmarathon-runtime",
    ]
    for scanned_root in scanned_roots:
        if not scanned_root.exists():
            continue
        for path in scanned_root.rglob("*"):
            if not path.is_file() or path.stat().st_size > 2 * 1024 * 1024:
                continue
            text = path.read_text(encoding="utf-8", errors="replace")
            if re.search(r"(?:^|[\\/])donor(?:[\\/])", text, re.IGNORECASE):
                # Provenance comments are expected in controller quota code and
                # docs; only executable manifests are forbidden.
                if path.name in {"Cargo.toml", "go.mod"}:
                    fail(f"product build manifest contains donor reference: {path.relative_to(root)}")

    messages.append(f"embedded Codext commit {codext_commit} and required notices verified")
    return messages


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--manifest", type=str)
    parser.add_argument(
        "--require-codex-switch-clearance",
        action="store_true",
        help="fail while the manifest still records unresolved codex-switch redistribution clearance",
    )
    args = parser.parse_args(argv)
    root = args.root.resolve()
    try:
        manifest, manifest_path = load_manifest(root, args.manifest)
        messages = validate(root, manifest, args.require_codex_switch_clearance)
    except (OSError, RuntimeError) as error:
        print(f"FAIL: provenance: {error}")
        return 1
    print(f"PASS: provenance: {manifest_path.relative_to(root)}")
    for message in messages:
        print(f"INFO: provenance: {message}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
