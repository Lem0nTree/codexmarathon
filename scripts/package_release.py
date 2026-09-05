#!/usr/bin/env python3
"""Assemble and verify a CodexMarathon companion release archive.

Only explicit product inputs are copied. The script never walks donor/, build
trees, controller state, or a user's Codex home, which makes accidental secret
inclusion materially harder than a broad directory archive. The normal mode
copies only the Go companion; the embedded runtime requires an explicit opt-in.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import sys
import tarfile
import tempfile
import zipfile
from datetime import datetime, timezone
from pathlib import Path, PurePosixPath

SCRIPT_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPT_DIR))
from verify_package import (  # noqa: E402
    check_secret_text,
    expected_required_files,
    safe_relative,
    validate_archive,
)


def fail(message: str) -> None:
    raise RuntimeError(message)


def load_manifest(root: Path, path_arg: str | None) -> dict:
    path = Path(path_arg) if path_arg else root / "packaging" / "release-manifest.json"
    if not path.is_absolute():
        path = root / path
    try:
        manifest = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        fail(f"cannot read release manifest: {error}")
    if not isinstance(manifest, dict) or manifest.get("schema_version") != 1:
        fail("release manifest schema_version must be 1")
    return manifest


def resolved_input(root: Path, value: str, label: str, *, allow_build_output: bool = False) -> Path:
    raw = Path(value)
    path = raw if raw.is_absolute() else root / raw
    path = path.resolve()
    try:
        path.relative_to(root.resolve())
    except ValueError:
        fail(f"{label} must be inside the repository: {value}")
    if not path.is_file():
        fail(f"{label} does not exist: {path}")
    forbidden = {"donor", ".git", ".codexmarathon", ".state"}
    if not allow_build_output:
        forbidden.add("target")
    if any(part.lower() in forbidden for part in path.relative_to(root).parts):
        fail(f"{label} points into a forbidden source tree: {path}")
    return path


def digest(path: Path) -> tuple[int, str]:
    hasher = hashlib.sha256()
    size = 0
    with path.open("rb") as stream:
        while chunk := stream.read(1024 * 1024):
            hasher.update(chunk)
            size += len(chunk)
    return size, hasher.hexdigest()


def copy_file(root: Path, staging_root: Path, source: Path, destination: str) -> None:
    safe = safe_relative(destination)
    target = staging_root / Path(*safe.parts)
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, target)
    check_secret_text(str(safe), target.read_bytes())


def make_generated_manifest(
    staging_root: Path,
    artifact_root: str,
    manifest: dict,
    version: str,
    platform: str,
    distribution: str,
) -> None:
    records = []
    for path in sorted(staging_root.rglob("*")):
        if not path.is_file():
            continue
        relative = path.relative_to(staging_root).as_posix()
        if relative == "release-manifest.json":
            continue
        size, sha256 = digest(path)
        records.append({"path": relative, "size": size, "sha256": sha256})
    generated = {
        "schema_version": 1,
        "product": manifest["product"],
        "version": version,
        "platform": platform,
        "distribution": distribution,
        "artifact_root": artifact_root,
        "created_at": datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
        "provenance": manifest.get("provenance", {}),
        "files": records,
    }
    output = staging_root / "release-manifest.json"
    output.write_text(json.dumps(generated, indent=2, sort_keys=True) + "\n", encoding="utf-8", newline="\n")


def archive_directory(staging_parent: Path, artifact_root: str, output: Path, archive_format: str) -> None:
    output.parent.mkdir(parents=True, exist_ok=True)
    if output.exists():
        fail(f"refusing to overwrite existing release artifact: {output}; remove it explicitly")
    if archive_format == "zip":
        with zipfile.ZipFile(output, "w", compression=zipfile.ZIP_DEFLATED) as archive:
            for path in sorted((staging_parent / artifact_root).rglob("*")):
                if path.is_file():
                    archive.write(path, PurePosixPath(artifact_root, path.relative_to(staging_parent / artifact_root)).as_posix())
    elif archive_format == "tar.gz":
        with tarfile.open(output, "w:gz") as archive:
            archive.add(staging_parent / artifact_root, arcname=artifact_root, recursive=True, filter=tar_filter)
    else:
        fail(f"unsupported archive format: {archive_format}")


def tar_filter(member: tarfile.TarInfo) -> tarfile.TarInfo:
    # Preserve executable bits but normalize ownership and timestamps so an
    # archive never leaks the builder's username or host metadata.
    member.uid = 0
    member.gid = 0
    member.uname = ""
    member.gname = ""
    member.mtime = 0
    return member


def build(args: argparse.Namespace) -> Path:
    root = args.root.resolve()
    manifest = load_manifest(root, args.manifest)
    version = args.version.strip()
    platform = args.platform.strip()
    if not version or any(char in version for char in "/\\"):
        fail("version must be a non-empty path-safe value")
    if not platform or any(char in platform for char in "/\\"):
        fail("platform must be a non-empty path-safe value")
    controller_binary = resolved_input(root, args.controller_binary, "controller binary")
    runtime_argument = getattr(args, "runtime_binary", None)
    include_embedded_runtime = bool(getattr(args, "include_embedded_runtime", False))
    if include_embedded_runtime and not runtime_argument:
        fail("--include-embedded-runtime requires --runtime-binary")
    if not include_embedded_runtime and runtime_argument:
        fail("--runtime-binary requires the explicit --include-embedded-runtime opt-in")
    runtime_binary = None
    if include_embedded_runtime:
        runtime_binary = resolved_input(root, runtime_argument, "runtime binary", allow_build_output=True)
    include = expected_required_files(manifest)
    source_paths = {relative: resolved_input(root, relative, "allow-listed release input") for relative in include}
    artifact_root = str(manifest.get("artifact_root", "codexmarathon-{version}-{platform}")).format(version=version, platform=platform)
    if not artifact_root or "/" in artifact_root or "\\" in artifact_root or artifact_root in {".", ".."}:
        fail("manifest artifact_root is not a safe directory name")
    extension = ".zip" if args.format == "zip" else ".tar.gz"
    output = args.output.resolve() / f"{artifact_root}{extension}"
    with tempfile.TemporaryDirectory(prefix=f"{artifact_root}-", dir=str(args.output.resolve().parent if args.output.resolve().parent.exists() else root)) as temporary:
        staging_parent = Path(temporary)
        staging_root = staging_parent / artifact_root
        staging_root.mkdir()
        copy_file(root, staging_root, controller_binary, manifest["entrypoints"]["controller"] + (".exe" if args.windows_exe else ""))
        if runtime_binary is not None:
            optional_entrypoints = manifest.get("optional_entrypoints", {})
            runtime_name = optional_entrypoints.get("runtime")
            if not isinstance(runtime_name, str) or not runtime_name.strip():
                fail("manifest optional_entrypoints.runtime is required for embedded-runtime packages")
            copy_file(root, staging_root, runtime_binary, runtime_name + (".exe" if args.windows_exe else ""))
        for relative, source in sorted(source_paths.items()):
            copy_file(root, staging_root, source, relative)
        entrypoint_names = {manifest["entrypoints"]["controller"]}
        if runtime_binary is not None:
            entrypoint_names.add(runtime_name)
        entrypoint_names |= {name + ".exe" for name in entrypoint_names}
        for path in staging_root.rglob("*"):
            if path.is_file() and path.name in entrypoint_names:
                if os.name != "nt":
                    path.chmod(0o755)
        distribution = "companion-with-embedded-runtime" if include_embedded_runtime else "companion"
        make_generated_manifest(staging_root, artifact_root, manifest, version, platform, distribution)
        archive_directory(staging_parent, artifact_root, output, args.format)
    # Re-read the final archive. This validates the exact bytes handed to the
    # caller rather than merely trusting the temporary staging directory.
    validate_archive(output, manifest)
    return output


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--manifest", type=str)
    parser.add_argument("--output", type=Path, default=Path("dist/release"))
    parser.add_argument("--controller-binary", required=True)
    parser.add_argument(
        "--runtime-binary",
        help="embedded Codex app-server binary; usable only with --include-embedded-runtime",
    )
    parser.add_argument(
        "--include-embedded-runtime",
        action="store_true",
        help="explicitly produce the optional self-contained package variant",
    )
    parser.add_argument("--platform", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--format", choices=("tar.gz", "zip"), default="tar.gz")
    parser.add_argument("--windows-exe", action="store_true", help="append .exe to packaged entrypoint names")
    args = parser.parse_args(argv)
    try:
        artifact = build(args)
    except (OSError, RuntimeError, KeyError) as error:
        print(f"FAIL: package release: {error}")
        return 1
    print(f"PASS: package release: {artifact}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
