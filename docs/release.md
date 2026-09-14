# Release validation

CodexMarathon releases are custom Codex CLI builds with native Marathon
controls compiled into the `codex` executable. Release archives include the
account daemon, its automatic installer and user systemd units, plus every
sibling executable the Codex CLI package expects:

- Linux: `codex`, `codexmarathon-accountd`, `codex-code-mode-host`,
  `codex-responses-api-proxy`, and `bwrap`. Linux ARM64 omits the unavailable
  code-mode host and responses proxy.
- Windows: `codex.exe`, `codex-code-mode-host.exe`,
  `codex-responses-api-proxy.exe`, `codex-command-runner.exe`, and
  `codex-windows-sandbox-setup.exe`.

Do not publish a CLI-only archive. The quota display depends on accountd, and
code mode and platform sandboxing depend on their sibling resources. The
release installer installs and enables both accountd user units, starts the
daemon, and verifies it is active; installation fails instead of silently
leaving manual activation work. It also enables user lingering so the service
continues running after a headless SSH session exits when local policy permits
the user or passwordless sudo to do so. If neither path is authorized, the
installer fails before copying files instead of claiming a non-persistent
daemon installation succeeded.

The release installer uses `$HOME/.codex` by default. For a different state
root, `CODEXMARATHON_CODEX_HOME` takes precedence over an existing
`CODEX_HOME`; when neither is set, the default is used. If both environment
variables are present, their normalized values must match. The selected value
must be an absolute printable path (not `/`, with no `%`, `.` or `..` path
components). Spaces, quotes, and backslashes are escaped in the generated
`codexmarathon-accountd.service.d/10-codex-home.conf` drop-in. The drop-in
sets the daemon environment and replaces both filesystem allowlists, while
the socket remains under `%t/codexmarathon-accountd/accountd.sock`. Unsetting
both variables on a later upgrade removes the drop-in and restores the
packaged default. The installer also atomically records the choice in the
owner-private `$XDG_CONFIG_HOME/codexmarathon/config.json` (falling back to
`$HOME/.config`), which the modified CLI and daemon resolve automatically.
It does not alter shell startup files.

Registry mutations in v0.155.0 and later upgrade the Marathon registry from
schema v1 to v2 for fresh credential references and batch commits. New builds
read v1, while older builds reject v2. Stop old CodexMarathon CLI processes
before installing this release, and do not downgrade after mutating account
state unless restoring a complete compatible state backup.

## Publishing releases

Releases are currently built and published manually from a clean checkout of
the exact commit being tagged. Set `CODEXMARATHON_VERSION` to the intended tag,
run the required checks below on the target architecture, and run
`scripts/build-release.sh`. Do not tag or publish a build produced from a dirty
or different source tree.

For Linux ARM64, publish the generated
`codexmarathon-<version>-linux-aarch64.tar.gz` archive and its `.sha256` file.
The public `scripts/install.sh` bootstrap selects that suffix for ARM64 hosts.
Before publication, install the archive on an acceptance host and verify the
CLI version, enabled accountd socket/service, API health, account quota data,
and restart persistence. The Git tag and GitHub release must point to the same
commit used for the tested archive.

## Required checks

From the repository root:

```bash
sh -n scripts/install.sh scripts/install-release.sh scripts/test-install-release.sh
scripts/test-install-release.sh
python3 scripts/verify_provenance.py --root .
cd runtime/codex-rs
cargo fmt --all -- --check
cargo test --locked -p codexmarathon-runtime
cargo test --locked -p codexmarathon-home -p codexmarathon-transfer
cd ../..
scripts/build-release.sh
```

The build script creates a complete archive under `dist/release` and verifies
the Marathon command surface. For an additional local check:

```bash
dist/release/codex marathon --help
dist/release/codex marathon status
```

## Live acceptance

Use a disposable `CODEX_HOME` and a real ChatGPT account only in an explicit
acceptance environment. Record the executable version and exercise:

1. `codex marathon login ALIAS --device-code` on a headless host.
2. A second account import or login under another alias.
3. `codex marathon status`, `on`, `off`, and `switch ALIAS`.
4. `/marathon` help and status inside an interactive session.
5. Status-line Marathon state and managed-account count.
6. Recovery at a safe turn boundary with no duplicate continuation.
7. Encrypted export, wrong-passphrase/tamper rejection, dry-run import, and an
   exact synthetic round trip between isolated Codex homes.

Do not invoke a real quota reset while testing automatic reset. The product
uses mocked reset capabilities and executors until a provider integration is
explicitly reviewed and enabled.

## Provenance

The embedded Codex source and local Marathon additions are documented in
[`runtime/PROVENANCE.md`](../runtime/PROVENANCE.md) and
[`runtime/PATCH_LEDGER.md`](../runtime/PATCH_LEDGER.md). Do not package local
credentials, unrelated Cargo target output, or developer caches in a release.
