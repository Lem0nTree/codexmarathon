#!/usr/bin/env python3
"""Installer regression checks with a real CLI TOML editor and stubbed services.

Usage: python3 scripts/test-install-startup.py /path/to/release/codex
No user services, credentials, network calls, or model requests are used.
"""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import tomllib


cli = Path(sys.argv[1]).resolve()
repo = Path(__file__).resolve().parent.parent


def executable(path, text):
    path.write_text(text)
    path.chmod(0o755)


cases = {
    "fresh": None,
    "empty": "",
    "table": '[features]\nshell_tool = true\n',
    "explicit_true": '[features]\ndaemon_auto_start = true\n',
    "explicit_false": '[features]\ndaemon_auto_start = false\n',
    "dotted": 'features.shell_tool = true\n',
    "inline": 'features = { shell_tool = true, daemon_auto_start = true }\n',
    "quoted": '["features"]\n"daemon_auto_start" = true\n',
    "profile": '[profiles.other.features]\ndaemon_auto_start = true\n',
    "custom_home": '[features] # existing settings\nshell_tool = true\n',
    "retry": None,
    "unhealthy_api": None,
    "stop_failure": None,
    "invalid_toml": '[features\n',
}

for name, config in cases.items():
    with tempfile.TemporaryDirectory(prefix="marathon-startup-test-") as tmp:
        root = Path(tmp)
        home, package, stubs = [root / part for part in ("home", "package", "bin")]
        state = root / 'custom "Codex" home' if name == "custom_home" else home / ".codex"
        for path in (home, package / "systemd/user", stubs, state):
            path.mkdir(parents=True, exist_ok=True)
        original = tomllib.loads(config or "") if name != "invalid_toml" else None
        if config is not None:
            (state / "config.toml").write_text(config)
        for unit in ("codexmarathon-accountd.service", "codexmarathon-accountd.socket", "codexmarathon-reset-executor.service"):
            (package / "systemd/user" / unit).write_bytes((repo / "infra/systemd/user" / unit).read_bytes())
        executable(stubs / "uname", '#!/bin/sh\necho aarch64\n')
        executable(stubs / "loginctl", '#!/bin/sh\necho yes\n')
        executable(stubs / "systemctl", '''#!/usr/bin/env python3
import os, sys
from pathlib import Path
p = Path(os.environ["TEST_ROOT"])
args = sys.argv[1:]
if "reset-failed" in args:
    (p / "reset").touch()
if "enable" in args:
    if not (p / "reset").exists():
        sys.exit("Start request repeated too quickly")
    (p / "enabled").touch()
''')
        executable(package / "codexmarathon-accountd", '#!/bin/sh\nexit 0\n')
        executable(package / "codexmarathon-reset-executor", '#!/bin/sh\nexit 0\n')
        executable(package / "codex-code-mode-host", '#!/bin/sh\nexit 0\n')
        executable(package / "codex", '''#!/usr/bin/env python3
import json, os, subprocess, sys
from pathlib import Path
p = Path(os.environ["TEST_ROOT"])
args = sys.argv[1:]
with (p / "calls").open("a") as f:
    f.write(json.dumps([args, os.environ.get("CODEX_HOME")]) + "\\n")
if args == ["features", "disable", "daemon_auto_start"]:
    sys.exit(subprocess.call([os.environ["TEST_CLI"], *args]))
if args == ["app-server", "daemon", "stop"]:
    sys.exit(1 if os.environ["TEST_CASE"] == "stop_failure" else 0)
if args == ["marathon", "accounts", "--daemon", "--format", "json"]:
    n = p / "attempts"
    count = int(n.read_text()) + 1 if n.exists() else 1
    n.write_text(str(count))
    sys.exit(1 if os.environ["TEST_CASE"] == "unhealthy_api" or
             (os.environ["TEST_CASE"] == "retry" and count < 3) else 0)
sys.exit("Unexpected CLI invocation")
''')
        env = os.environ.copy()
        for key in ("CODEX_HOME", "CODEXMARATHON_CODEX_HOME", "CODEXMARATHON_INSTALL_DIR", "XDG_CONFIG_HOME"):
            env.pop(key, None)
        env.update(HOME=str(home), PATH=str(stubs) + os.pathsep + env["PATH"],
                   TEST_ROOT=tmp, TEST_CASE=name, TEST_CLI=str(cli))
        if name == "custom_home":
            env["CODEXMARATHON_CODEX_HOME"] = str(state)
        result = subprocess.run(["sh", str(repo / "scripts/install-release.sh"), str(package)],
                                env=env, capture_output=True, text=True)
        expected_failure = name in ("stop_failure", "invalid_toml", "unhealthy_api")
        assert (result.returncode != 0) == expected_failure, (name, result.stderr)
        if name == "invalid_toml":
            assert (state / "config.toml").read_text() == config
            assert not (root / "enabled").exists()
        else:
            edited = tomllib.loads((state / "config.toml").read_text())
            original.setdefault("features", {})["daemon_auto_start"] = False
            assert edited == original, (name, edited)
            assert (state / "config.toml").stat().st_mode & 0o777 == 0o600
            assert not (state / "auth.json").exists()
            assert not (state / "marathon/vault").exists()
            calls = [json.loads(line) for line in (root / "calls").read_text().splitlines()]
            assert all(call[1] == str(state) for call in calls), (name, calls)
            assert ["app-server", "daemon", "stop"] in [call[0] for call in calls]
            if name in ("retry", "unhealthy_api"):
                assert (root / "attempts").read_text() == "3"
            unit_path = home / ".config/systemd/user/codexmarathon-accountd.service"
            assert "InaccessiblePaths=-%h/.codex/auth.json -%h/.codex/marathon/vault" in unit_path.read_text()
        print(f"PASS {name}")
print(f"Passed {len(cases)} installer startup scenarios")
