#!/usr/bin/env python3
"""Verify source release inputs or an assembled CodexMarathon archive."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
import tarfile
import zipfile
from pathlib import Path, PurePosixPath


SECRET_PATTERNS = (
    re.compile(r"\bgh[pousr]_[A-Za-z0-9_\-]{20,}\b"),
    re.compile(r"\bgithub_pat_[A-Za-z0-9_\-]{20,}\b"),
    re.compile(r"\bsk-[A-Za-z0-9]{20,}\b"),
    re.compile(r"(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{24,}"),
)
FORBIDDEN_SEGMENTS = {
    ".git",
    ".codexmarathon",
    ".codexmarathon-dev",
    ".state",
    ".venv",
    "__pycache__",
    "donor",
    "node_modules",
    "target",
}
FORBIDDEN_FILES = {
    ".env",
    ".env.local",
    "auth.json",
    "accounts.json",
    "events.jsonl",
    "diagnostics.json",
}


def fail(message: str) -> None:
    raise RuntimeError(message)


def load_manifest(root: Path, path_arg: str | None) -> dict:
    path = Path(path_arg) if path_arg else root / "packaging" / "release-manifest.json"
    if not path.is_absolute():
        path = root / path
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        fail(f"cannot read release manifest {path}: {error}")
    if not isinstance(value, dict) or value.get("schema_version") != 1:
        fail("release manifest schema_version must be 1")
    return value


def safe_relative(value: str) -> PurePosixPath:
    normalized = value.replace("\\", "/")
    path = PurePosixPath(normalized)
    if not normalized or str(path) in {"", "."}:
        fail(f"archive path is empty: {value!r}")
    if path.is_absolute() or ".." in path.parts:
        fail(f"archive path escapes its root: {value!r}")
    if any(segment.lower() in FORBIDDEN_SEGMENTS for segment in path.parts):
        fail(f"forbidden path segment in release artifact: {value!r}")
    if path.name.lower() in FORBIDDEN_FILES:
        fail(f"forbidden state/credential file in release artifact: {value!r}")
    return path


def digest_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def check_secret_text(name: str, data: bytes) -> None:
    if len(data) > 8 * 1024 * 1024:
        return
    text = data.decode("utf-8", errors="ignore")
    for pattern in SECRET_PATTERNS:
        if pattern.search(text):
            fail(f"possible credential material detected in release file: {name}")


def expected_required_files(manifest: dict) -> set[str]:
    include = {str(value).replace("\\", "/") for value in manifest.get("include", [])}
    if not include:
        fail("release manifest include list is empty")
    return include


def validate_source(root: Path, manifest: dict) -> list[str]:
    required = expected_required_files(manifest)
    for relative in required:
        safe_relative(relative)
        path = root / Path(relative)
        if not path.is_file():
            fail(f"release input is missing: {relative}")
    return sorted(required)


def read_archive(path: Path) -> dict[str, bytes]:
    if path.suffix.lower() == ".zip":
        with zipfile.ZipFile(path) as archive:
            result: dict[str, bytes] = {}
            for info in archive.infolist():
                if info.is_dir():
                    continue
                safe = safe_relative(info.filename)
                if safe.as_posix() in result:
                    fail(f"duplicate archive member: {info.filename}")
                result[str(safe)] = archive.read(info)
            return result
    if path.name.endswith(".tar.gz") or path.name.endswith(".tgz") or path.suffix.lower() == ".tar":
        mode = "r:gz" if path.name.endswith((".tar.gz", ".tgz")) else "r:"
        with tarfile.open(path, mode) as archive:
            result = {}
            for member in archive.getmembers():
                if member.isdir():
                    continue
                if member.issym() or member.islnk():
                    fail(f"symbolic links are not allowed in release archives: {member.name}")
                safe = safe_relative(member.name)
                if safe.as_posix() in result:
                    fail(f"duplicate archive member: {member.name}")
                extracted = archive.extractfile(member)
                if extracted is None:
                    fail(f"archive member is not readable: {member.name}")
                result[str(safe)] = extracted.read()
            return result
    fail(f"unsupported release artifact format: {path}")


def validate_archive(path: Path, manifest: dict) -> list[str]:
    files = read_archive(path)
    required = expected_required_files(manifest)
    # Archives have a generated root directory. Match required metadata and
    # notices by suffix so the verifier is independent of version/platform.
    by_suffix: dict[str, tuple[str, bytes]] = {}
    artifact_roots: set[str] = set()
    for name, data in files.items():
        parts = PurePosixPath(name).parts
        if len(parts) < 2:
            fail(f"release archive member is not inside an artifact root: {name}")
        artifact_roots.add(parts[0])
        suffix = "/".join(parts[1:])
        if suffix in by_suffix:
            fail(f"duplicate artifact member after root stripping: {suffix}")
        by_suffix[suffix] = (name, data)
        check_secret_text(name, data)
    if len(artifact_roots) != 1:
        fail(f"release archive must contain exactly one artifact root: {sorted(artifact_roots)}")
    for required_path in required:
        if required_path not in by_suffix:
            fail(f"required release file is missing from archive: {required_path}")
    entrypoints = manifest.get("entrypoints", {})
    for role, expected_name in entrypoints.items():
        candidates = [suffix for suffix in by_suffix if PurePosixPath(suffix).name == expected_name or PurePosixPath(suffix).name == expected_name + ".exe"]
        if not candidates:
            fail(f"release entrypoint {role!r} is missing ({expected_name})")
    manifest_candidates = [value for suffix, value in by_suffix.items() if PurePosixPath(suffix).name == "release-manifest.json"]
    if len(manifest_candidates) != 1:
        fail("archive must contain exactly one generated release-manifest.json")
    try:
        generated = json.loads(manifest_candidates[0][1].decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        fail(f"generated release manifest is invalid: {error}")
    if generated.get("product") != manifest.get("product"):
        fail("generated release manifest product does not match source manifest")
    records = generated.get("files")
    if not isinstance(records, list):
        fail("generated release manifest has no file hash records")
    record_paths: set[str] = set()
    for record in records:
        if not isinstance(record, dict) or not isinstance(record.get("path"), str):
            fail("generated release manifest contains an invalid file record")
        suffix = safe_relative(record["path"]).as_posix()
        if suffix == "release-manifest.json" or suffix in record_paths:
            fail(f"generated release manifest contains a duplicate/reserved path: {suffix}")
        record_paths.add(suffix)
        if suffix not in by_suffix:
            fail(f"generated file hash names a missing member: {suffix}")
        data = by_suffix[suffix][1]
        if record.get("sha256") != digest_bytes(data):
            fail(f"SHA-256 mismatch for generated member: {suffix}")
    actual_paths = set(by_suffix) - {"release-manifest.json"}
    if record_paths != actual_paths:
        fail(
            "generated release manifest does not enumerate exactly the archive files: "
            f"missing={sorted(actual_paths - record_paths)}, "
            f"unrecorded={sorted(record_paths - actual_paths)}"
        )
    return sorted(files)


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--manifest", type=str)
    parser.add_argument("--artifact", type=Path)
    args = parser.parse_args(argv)
    root = args.root.resolve()
    try:
        manifest = load_manifest(root, args.manifest)
        if args.artifact:
            checked = validate_archive(args.artifact.resolve(), manifest)
            print(f"PASS: release archive: {args.artifact} ({len(checked)} files)")
        else:
            checked = validate_source(root, manifest)
            print(f"PASS: release inputs: {len(checked)} allow-listed files and notices present")
    except (OSError, RuntimeError) as error:
        print(f"FAIL: release package: {error}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
