#!/usr/bin/env python3
"""Exercise compiled expiry daemon/executor using disposable fake native auth.

Usage: test-reset-expiry.py /path/to/codexmarathon-accountd /path/to/codexmarathon-reset-executor [/path/to/codex]
Only a localhost mock provider is called; no real accounts, installed services,
production credentials, or production state are used.
"""
import base64
import datetime as dt
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import time


def stamp(offset=0):
    return (dt.datetime.now(dt.timezone.utc) + dt.timedelta(seconds=offset)).isoformat()


def fake_jwt(account_id):
    """Native auth parses these disposable test claims; no signing key exists."""
    def encode(value):
        return base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip("=")
    return ".".join((encode({"alg": "none", "typ": "JWT"}), encode({
        "exp": int(time.time()) + 3600, "email": "fixture@example.invalid",
        "https://api.openai.com/auth": {
            "chatgpt_account_id": account_id, "chatgpt_user_id": "fixture-user",
            "chatgpt_plan_type": "pro",
        },
    }), "fixture"))


class MockProvider(BaseHTTPRequestHandler):
    """A failing first consume and successful second consume, on localhost only."""
    def log_message(self, *args):
        pass

    def reply(self, code, body):
        payload = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def authenticated(self):
        valid = (self.headers.get("Authorization") == "Bearer fixture-access"
                 and self.headers.get("chatgpt-account-id") == "fake-account")
        if not valid:
            self.server.failures.append("mock request had unexpected account/auth headers")
            self.reply(401, {"error": "fixture auth mismatch"})
        return valid

    def do_GET(self):
        if not self.authenticated():
            return
        if self.path == "/backend-api/wham/rate-limit-reset-credits":
            self.reply(200, {"available_count": 0 if self.server.redeemed else 1,
                             "credits": [{
                                 "id": "provider-credit", "reset_type": "codex_rate_limits",
                                 "status": "redeemed" if self.server.redeemed else "available",
                                 "granted_at": self.server.granted_at,
                                 "expires_at": self.server.expires_at,
                                 "title": None, "description": None,
                             }]})
        elif self.path == "/backend-api/wham/usage":
            self.server.quota_reads += 1
            self.reply(200, {"account_id": "fake-account", "plan_type": "pro",
                             "rate_limit": {"allowed": True, "limit_reached": False,
                                            "primary_window": {
                                                "used_percent": 0,
                                                "limit_window_seconds": 18000,
                                                "reset_after_seconds": 18000,
                                                "reset_at": int(time.time()) + 18000,
                                            }}})
        else:
            self.server.failures.append("unexpected mock GET " + self.path)
            self.reply(404, {"error": "unknown fixture path"})

    def do_POST(self):
        if not self.authenticated():
            return
        if self.path != "/backend-api/wham/rate-limit-reset-credits/consume":
            self.server.failures.append("unexpected mock POST " + self.path)
            self.reply(404, {"error": "unexpected refresh or provider request"})
            return
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        self.server.consumes.append(body)
        if len(self.server.consumes) == 1:
            self.reply(503, {"error": "fixture transient failure"})
        else:
            already = self.server.redeemed
            self.server.redeemed = True
            self.reply(200, {"code": "already_redeemed" if already else "reset",
                             "windows_reset": 0 if already else 1})


daemon_binary, executor_binary = map(lambda p: str(Path(p).resolve()), sys.argv[1:3])
with tempfile.TemporaryDirectory(prefix="marathon-expiry-smoke-") as temporary:
    root = Path(temporary)
    home = root / "home"
    state = home / "marathon"
    state.mkdir(parents=True, mode=0o700)
    home.chmod(0o700)
    runtime = root / "runtime"
    socket_dir = runtime / "codexmarathon-accountd"
    socket_dir.mkdir(parents=True, mode=0o700)
    address = str(socket_dir / "accountd.sock")
    registry = {
        "version": 2, "enabled": True,
        "accounts": {"fake-account": {
            "id": "fake-account", "alias": "fixture",
            "created_at": stamp(), "updated_at": stamp(),
        }},
    }
    registry_path = state / "accounts.json"
    registry_path.write_text(json.dumps(registry))
    registry_path.chmod(0o600)
    processes = []
    provider = None
    provider_thread = None

    def start_daemon():
        process = subprocess.Popen([
            daemon_binary, "--codex-home", str(home), "--socket", address,
        ], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        processes.append(process)
        for _ in range(100):
            if process.poll() is not None:
                raise AssertionError("daemon exited before readiness")
            try:
                rpc("health")
                return process
            except (OSError, AssertionError):
                time.sleep(0.05)
        raise AssertionError("daemon readiness timed out")

    def rpc(method, params=None, ok=True):
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as stream:
            stream.settimeout(5)
            stream.connect(address)
            stream.sendall(json.dumps({"version": 1, "id": 1, "method": method,
                                       "params": params}).encode() + b"\n")
            response = json.loads(stream.makefile("rb").readline())
        assert response["ok"] is ok, response
        return response.get("result")

    def toggle(enabled, expected=None):
        return rpc("auto_reset_expiry_set", {
            "enabled": enabled, "expected_codex_home": str(expected or home),
        })

    try:
        daemon = start_daemon()
        assert rpc("auto_reset_expiry_status")["enabled"] is False
        if len(sys.argv) > 3:
            cli = str(Path(sys.argv[3]).resolve())
            env = os.environ.copy()
            for key in ("OPENAI_API_KEY", "CODEX_API_KEY", "CODEXMARATHON_CODEX_HOME"):
                env.pop(key, None)
            env.update(HOME=str(home), CODEX_HOME=str(home), XDG_RUNTIME_DIR=str(runtime))
            for action, expected in (("status", False), ("on", True), ("off", False)):
                result = subprocess.run([cli, "marathon", "auto-reset-expiry", action],
                                        cwd=home, env=env, capture_output=True, text=True,
                                        timeout=30)
                assert result.returncode == 0, (action, result.stderr)
                assert rpc("auto_reset_expiry_status")["enabled"] is expected
            print("PASS CLI → native app-server API → daemon status/on/off")

        rpc("auto_reset_expiry_set", {"enabled": True,
                                    "expected_codex_home": str(root)}, ok=False)
        toggle(True)
        rpc("expiry_executor_heartbeat")
        rpc("expiry_observe", {"account_id": "fake-account", "observed_at": stamp(),
                              "credits": [{"credit_id": "credit", "status": "available",
                                           "expires_at": stamp(890)}]})
        first = rpc("expiry_claim")
        assert first["attempt"] == 1
        assert rpc("expiry_claim") is None, "live lease was reclaimed"
        rpc("expiry_complete", {"job_id": first["job_id"], "lease_token": first["lease_token"],
                                "outcome": "retry", "diagnostic_code": "fixture_timeout"})
        assert rpc("expiry_claim") is None, "backoff was skipped"
        deadline = time.monotonic() + 8
        second = None
        while second is None and time.monotonic() < deadline:
            time.sleep(0.1)
            second = rpc("expiry_claim")
        assert second is not None, "retry did not become due"
        assert second["idempotency_key"] == first["idempotency_key"]
        assert second["lease_token"] != first["lease_token"]
        rpc("expiry_complete", {"job_id": first["job_id"], "lease_token": first["lease_token"],
                                "outcome": "reset", "diagnostic_code": None}, ok=False)
        toggle(False)
        rpc("expiry_complete", {"job_id": second["job_id"], "lease_token": second["lease_token"],
                                "outcome": "already_redeemed", "diagnostic_code": None})
        daemon.terminate()
        daemon.wait(timeout=10)
        daemon = start_daemon()
        status = rpc("auto_reset_expiry_status")
        assert status["enabled"] is False
        assert status["jobs"][0]["state"] == "succeeded"
        toggle(True)
        rpc("expiry_executor_heartbeat")
        assert rpc("expiry_claim") is None, "redeemed credit reopened after restart"
        toggle(False)
        executor = subprocess.Popen([executor_binary, "--codex-home", str(home),
                                     "--socket", address], stdout=subprocess.DEVNULL,
                                    stderr=subprocess.PIPE)
        processes.append(executor)
        time.sleep(0.5)
        assert executor.poll() is None
        assert not (home / "auth.json").exists()
        assert not (state / "vault").exists(), "disabled executor accessed credentials"
        print("PASS disabled default, home guard, due claim, backoff, stable retry key, "
              "lease fencing, disable in flight, durable success, disabled executor")

        # Stop the disabled process before installing fake inactive-account auth.
        executor.terminate()
        executor.wait(timeout=10)
        provider = ThreadingHTTPServer(("127.0.0.1", 0), MockProvider)
        provider.consumes, provider.failures = [], []
        provider.redeemed, provider.quota_reads = False, 0
        provider.granted_at, provider.expires_at = stamp(), stamp(890)
        provider_thread = threading.Thread(target=provider.serve_forever, daemon=True)
        provider_thread.start()
        endpoint = f"http://127.0.0.1:{provider.server_port}/backend-api"
        (home / "config.toml").write_text(
            f'chatgpt_base_url = "{endpoint}"\ncli_auth_credentials_store = "file"\n')
        vault = state / "vault"
        vault.mkdir(mode=0o700)
        snapshot = vault / "fake-account.json"
        snapshot.write_text(json.dumps({
            "auth_mode": "chatgpt", "OPENAI_API_KEY": None,
            "tokens": {"account_id": "fake-account", "id_token": fake_jwt("fake-account"),
                       "access_token": "fixture-access", "refresh_token": "fixture-refresh"},
            "last_refresh": stamp(),
        }))
        snapshot.chmod(0o600)
        # Fresh native auth avoids token rotation; any accidental refresh stays
        # on the mock server and fails the assertion instead of reaching a provider.
        fixture_env = {
            "PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(home),
            "CODEX_HOME": str(home), "XDG_RUNTIME_DIR": str(runtime),
            "XDG_CONFIG_HOME": str(root / "config"), "XDG_CACHE_HOME": str(root / "cache"),
            "CODEX_REFRESH_TOKEN_URL_OVERRIDE": endpoint + "/unexpected-refresh",
            "NO_PROXY": "127.0.0.1,localhost",
        }

        def start_fixture_executor():
            process = subprocess.Popen([executor_binary, "--codex-home", str(home),
                                        "--socket", address], cwd=home, env=fixture_env,
                                       stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            processes.append(process)
            return process

        def provider_job():
            return next((job for job in rpc("auto_reset_expiry_status")["jobs"]
                         if job["credit_id"] == "provider-credit"), None)

        toggle(True)
        executor = start_fixture_executor()
        deadline = time.monotonic() + 20
        success = None
        while time.monotonic() < deadline:
            assert executor.poll() is None, "compiled executor exited during mock scenario"
            assert not provider.failures, provider.failures
            success = provider_job()
            if success and success["state"] == "succeeded" and provider.quota_reads:
                break
            time.sleep(0.1)
        assert success and success["state"] == "succeeded", (
            "compiled executor did not redeem the mock credit", success,
            len(provider.consumes), provider.failures)
        assert len(provider.consumes) == 2, "expected one transient failure and one successful retry"
        first_request, retry_request = provider.consumes
        assert first_request == retry_request, "native retry changed credit or redemption key"
        assert first_request["credit_id"] == "provider-credit"
        assert first_request.get("redeem_request_id"), "native request omitted idempotency key"
        assert success["attempt"] == 2, success
        assert provider.quota_reads >= 1, "successful native redemption did not refresh quota"
        assert not (home / "auth.json").exists(), "inactive redemption switched the active native auth"
        assert json.loads(registry_path.read_text()).get("active_account_id", "") == ""
        executor.terminate()
        executor.wait(timeout=10)
        daemon.terminate()
        daemon.wait(timeout=10)
        daemon = start_daemon()
        restarted = provider_job()
        assert restarted["state"] == "succeeded" and restarted["attempt"] == 2, restarted
        executor = start_fixture_executor()
        time.sleep(3)
        assert executor.poll() is None
        assert provider_job()["state"] == "succeeded"
        assert len(provider.consumes) == 2, "restart consumed the successful credit again"
        assert not provider.failures, provider.failures
        assert not (home / "auth.json").exists(), "restart switched the active native auth"
        print("PASS compiled executor → localhost native backend: transient failure, identical "
              "credit/key retry, quota refresh, no identity switch, durable success after restart")
    finally:
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
        if provider is not None:
            provider.shutdown()
            provider.server_close()
            provider_thread.join(timeout=2)
