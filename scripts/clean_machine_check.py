#!/usr/bin/env python3
"""Exercise a release archive from an empty, toolchain-free directory.

The check intentionally stops at controller initialization/status.  It proves
that the packaged controller can create and read its own state without a
repository, donor checkout, Go/Rust toolchain, or the operator's real Codex
home.  The default companion archive does not need a bundled Codex runtime;
the optional embedded-runtime entrypoint is inspected when present.  It does
not claim OAuth, provider quota, or continuation success.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path, PurePosixPath

SCRIPT_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPT_DIR))
from verify_package import (  # noqa: E402
    expected_required_files,
    load_manifest,
    read_archive,
    safe_relative,
    validate_archive,
)


def fail(message: str) -> None:
    raise RuntimeError(message)


def extract_archive(artifact: Path, destination: Path) -> Path:
    files = read_archive(artifact)
    roots = {PurePosixPath(name).parts[0] for name in files}
    if len(roots) != 1:
        fail(f"release archive must contain one root directory: {sorted(roots)}")
    artifact_root = destination / next(iter(roots))
    for name, data in files.items():
        parts = PurePosixPath(name).parts
        relative = PurePosixPath(*parts[1:])
        safe_relative(relative.as_posix())
        target = destination / Path(*parts)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
    return artifact_root


def entrypoint(root: Path, expected: str) -> Path:
    matches = sorted(
        path
        for path in root.rglob("*")
        if path.is_file() and path.name in {expected, expected + ".exe"}
    )
    if len(matches) != 1:
        fail(f"expected exactly one packaged {expected} entrypoint, found {len(matches)}")
    return matches[0]


def optional_entrypoint(root: Path, expected: str) -> Path | None:
    matches = sorted(
        path
        for path in root.rglob("*")
        if path.is_file() and path.name in {expected, expected + ".exe"}
    )
    if len(matches) > 1:
        fail(f"expected at most one packaged optional {expected} entrypoint, found {len(matches)}")
    return matches[0] if matches else None


def run_controller(controller: Path, args: list[str], environment: dict[str, str]) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(
            [str(controller), *args],
            cwd=str(controller.parent),
            env=environment,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=30,
            check=False,
        )
    except OSError as error:
        fail(f"clean-machine controller launch failed: {error}")
    except subprocess.TimeoutExpired as error:
        fail(f"clean-machine controller command timed out: {error.cmd}")


def run_check(root: Path, artifact: Path, manifest: dict, skip_run: bool) -> None:
    validate_archive(artifact, manifest)
    with tempfile.TemporaryDirectory(prefix="codexmarathon-clean-") as temporary:
        extraction = Path(temporary) / "install"
        extraction.mkdir()
        installed = extract_archive(artifact, extraction)
        controller = entrypoint(installed, str(manifest["entrypoints"]["controller"]))
        optional = manifest.get("optional_entrypoints", {})
        runtime_name = optional.get("runtime") if isinstance(optional, dict) else None
        runtime = optional_entrypoint(installed, runtime_name) if isinstance(runtime_name, str) else None
        if runtime is None:
            print("INFO: clean-machine companion archive has no bundled Codex runtime")
        else:
            print(f"INFO: clean-machine found optional bundled runtime: {runtime.name}")
        if os.name != "nt":
            controller.chmod(0o755)
        if skip_run:
            print("PASS: clean-machine archive extraction and boundary checks")
            return

        state = Path(temporary) / "state"
        codex_home = Path(temporary) / "codex-home"
        codex_home.mkdir(mode=0o700)
        # An empty PATH is deliberate: init/status must not shell out to a
        # developer toolchain or discover a donor executable.
        environment = os.environ.copy()
        environment.update(
            {
                "PATH": "",
                "CODEX_HOME": str(codex_home),
                "CODEX_DISABLE_UPDATE_CHECK": "1",
            }
        )
        auth = codex_home / "auth.json"
        init = run_controller(
            controller,
            ["init", "--state-dir", str(state), "--auth", str(auth)],
            environment,
        )
        if init.returncode != 0:
            fail(f"clean-machine init failed ({init.returncode}): {init.stderr[-2000:]}")
        status = run_controller(
            controller,
            ["status", "--state-dir", str(state), "--auth", str(auth), "--json"],
            environment,
        )
        if status.returncode != 0:
            fail(f"clean-machine status failed ({status.returncode}): {status.stderr[-2000:]}")
        try:
            status_view = json.loads(status.stdout)
        except json.JSONDecodeError as error:
            fail(f"clean-machine status returned invalid JSON: {error}")
        if status_view.get("account_count") != 0 or status_view.get("runtime_connected"):
            fail(f"unexpected clean-machine status: {status_view}")
        accounts = run_controller(
            controller,
            ["accounts", "list", "--state-dir", str(state), "--auth", str(auth), "--json"],
            environment,
        )
        if accounts.returncode != 0:
            fail(f"clean-machine account listing failed ({accounts.returncode}): {accounts.stderr[-2000:]}")
        if json.loads(accounts.stdout) != []:
            fail("clean-machine account listing was not empty")
        print("PASS: clean-machine install, init, status, and account-list checks")


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--manifest", type=str)
    parser.add_argument("--artifact", type=Path, required=True)
    parser.add_argument(
        "--skip-run",
        action="store_true",
        help="only extract and validate the artifact; useful when no built controller exists",
    )
    args = parser.parse_args(argv)
    root = args.root.resolve()
    try:
        manifest = load_manifest(root, args.manifest)
        # Keep this explicit so the clean-machine check fails if the source
        # release contract loses its notices or required metadata.
        expected_required_files(manifest)
        run_check(root, args.artifact.resolve(), manifest, args.skip_run)
    except (OSError, RuntimeError, KeyError, json.JSONDecodeError) as error:
        print(f"FAIL: clean-machine release check: {error}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
