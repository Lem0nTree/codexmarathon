#!/usr/bin/env python3
"""Exercise an explicitly built optional embedded runtime over Unix IPC.

This check applies only to the opt-in self-contained package variant. It
proves that the optional app-server can start, expose the Marathon listener,
negotiate protocol v1, and answer an identity read. The normal companion
package uses the user's separately installed Codex executable and does not
run this binary. This smoke does not claim OAuth, quota, or task-continuation
success.
"""

from __future__ import annotations

import argparse
import json
import os
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path


def recv_line(connection: socket.socket, buffer: bytearray) -> tuple[dict, bytearray]:
    deadline = time.monotonic() + 10
    while b"\n" not in buffer:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise RuntimeError("timed out waiting for runtime protocol frame")
        connection.settimeout(remaining)
        chunk = connection.recv(65536)
        if not chunk:
            raise RuntimeError("runtime closed the IPC connection before replying")
        buffer.extend(chunk)
    line, _, rest = buffer.partition(b"\n")
    try:
        value = json.loads(line.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise RuntimeError(f"runtime returned invalid JSON: {error}") from error
    if not isinstance(value, dict):
        raise RuntimeError("runtime protocol frame is not an object")
    return value, bytearray(rest)


def request(connection: socket.socket, request_id: str, method: str, params: dict, buffer: bytearray) -> tuple[dict, bytearray]:
    payload = {
        "jsonrpc": "2.0",
        "id": request_id,
        "method": method,
        "params": params,
    }
    connection.sendall((json.dumps(payload, separators=(",", ":")) + "\n").encode("utf-8"))
    while True:
        value, buffer = recv_line(connection, buffer)
        # Runtime events may be emitted alongside a response. Ignore them for
        # this handshake but keep reading the same stream.
        if value.get("id") == request_id:
            return value, buffer


def run(binary: Path, timeout: float) -> int:
    if os.name == "nt":
        print(
            "BLOCKED: live Unix-socket smoke is not implemented on Windows; "
            "run the Windows-native named-pipe acceptance harness before claiming live IPC"
        )
        return 2
    if not binary.is_file():
        print(f"BLOCKED: optional embedded runtime binary not found: {binary}")
        return 2
    with tempfile.TemporaryDirectory(prefix="codexmarathon-live-") as temporary:
        root = Path(temporary)
        socket_path = root / "runtime.sock"
        codex_home = root / "codex-home"
        codex_home.mkdir(mode=0o700)
        env = os.environ.copy()
        env.update(
            {
                "CODEX_HOME": str(codex_home),
                "CODEXMARATHON_LISTEN": f"unix://{socket_path}",
                "CODEX_APP_SERVER_DISABLE_MANAGED_CONFIG": "1",
                "CODEX_DISABLE_UPDATE_CHECK": "1",
            }
        )
        process = subprocess.Popen(
            [str(binary), "--listen", "off"],
            cwd=str(binary.parent),
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        try:
            deadline = time.monotonic() + timeout
            while not socket_path.exists():
                if process.poll() is not None:
                    stderr = process.stderr.read().decode("utf-8", errors="replace") if process.stderr else ""
                    raise RuntimeError(f"embedded runtime exited with {process.returncode}: {stderr[-2000:]}")
                if time.monotonic() >= deadline:
                    raise RuntimeError("embedded runtime did not create its Unix socket")
                time.sleep(0.1)
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
                connection.settimeout(max(1.0, timeout))
                connection.connect(str(socket_path))
                buffer = bytearray()
                negotiate, buffer = request(connection, "smoke-1", "protocol/negotiate", {"supported_versions": [1]}, buffer)
                if negotiate.get("error") is not None or negotiate.get("result", {}).get("protocol_version") != 1:
                    raise RuntimeError(f"protocol negotiation failed: {negotiate}")
                identity, buffer = request(connection, "smoke-2", "runtime/identity/read", {}, buffer)
                result = identity.get("result")
                if identity.get("error") is not None or not isinstance(result, dict) or not result.get("runtime_id"):
                    raise RuntimeError(f"runtime identity read failed: {identity}")
                print(f"PASS: live optional embedded runtime smoke: runtime_id={result['runtime_id']} generation={result.get('auth_generation', 0)}")
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
            return 0
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime-binary", type=Path, required=True)
    parser.add_argument("--timeout", type=float, default=30.0)
    args = parser.parse_args(argv)
    try:
        return run(args.runtime_binary.resolve(), args.timeout)
    except (OSError, RuntimeError) as error:
        print(f"FAIL: live optional embedded runtime smoke: {error}")
        return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
