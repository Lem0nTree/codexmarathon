# Features added through v0.155.1

This document summarizes the CodexMarathon features implemented and released
during the accountd, quota-integration, custom-home, and encrypted-backup work.
They are included in the modified Codex CLI and its release archive; users do
not need to install a separate plugin or SDK.

Releases: [`v0.155.0`](https://github.com/Lem0nTree/codexmarathon/releases/tag/v0.155.0),
[`v0.155.1`](https://github.com/Lem0nTree/codexmarathon/releases/tag/v0.155.1)

## TUI and quota improvements in v0.155.1

- `/marathon` and `/marathon status` now use native Codex styling with bold
  headings, colored state/health values, and an aligned account table.
- `/marathon export` provides an integrated checkbox, output-filename, masked
  password, and password-confirmation workflow.
- Export passwords remain in zeroizing TUI-local state and never enter accountd,
  app-server RPCs, history, or debug-printable events.
- Five-hour and weekly windows are classified by their provider-reported
  duration. Weekly-only Pro accounts therefore show no five-hour allowance and
  report their quota in the weekly column.

## Metadata and quota service

CodexMarathon now ships `codexmarathon-accountd`, a long-running per-user
service for account metadata, quota observations, scheduling state, and
replayable events.

- Exposes an owner-only versioned API over an AF_UNIX socket.
- Reports managed account aliases, active state, credential health, five-hour
  quota, weekly quota, freshness, and reset time.
- Stores metadata, events, and job state in owner-private SQLite storage.
- Receives provider-authored quota observations from the native app server
  after Codex activity.
- Never reads or serves `auth.json`, saved credential snapshots, tokens,
  cookies, authorization headers, or arbitrary provider payloads.
- Has no TCP listener and cannot execute arbitrary commands.

Query the service from the CLI:

```bash
codex marathon accounts --daemon --format table
codex marathon accounts --daemon --format motd
codex marathon accounts --daemon --format json
```

The `motd` format is designed for an Armbian SSH welcome screen and includes
the five-hour and weekly quota columns.

## Typed accountd client library

The internal `codexmarathon-accountd-client` crate defines the supported
protocol and reusable typed data models. Custom integrations can use:

- `health()` for protocol and service readiness;
- `status()` for scheduler and storage status;
- `accounts()` for account and quota metadata;
- `events_since(id)` for durable incremental event consumption.

This keeps Unix-socket framing, request identifiers, protocol-version checks,
timeouts, size bounds, error mapping, and response decoding in one maintained
place. Future applications such as desktop indicators, notification bridges,
dashboards, and automation controllers can share this client rather than
implementing the wire protocol independently.

The client is a separate Rust crate for code organization, but it is still
built and distributed as part of the single CodexMarathon product. End users
do not install it separately.

## Automatic daemon installation

The normal CodexMarathon installer now installs the complete runtime in one
operation:

```bash
curl -fsSL https://raw.githubusercontent.com/Lem0nTree/codexmarathon/main/scripts/install.sh | sh
```

The release contains the modified `codex` binary, `codexmarathon-accountd`,
the platform sandbox helper, the archive installer, and the accountd systemd
user units. The installer:

- installs or upgrades the CLI and daemon binaries;
- installs, enables, starts, and verifies the accountd socket and service;
- enables user lingering when host policy permits, keeping the scheduler alive
  after the SSH session ends;
- creates private state/configuration directories;
- performs an accountd API check before reporting success;
- fails explicitly if it cannot establish a persistent working service.

No manual systemd setup or second package installation is required.

## Armbian SSH welcome integration

The repository includes `scripts/armbian-motd-codexmarathon.sh` for displaying
managed accounts in the Armbian welcome screen. It reads credential-free data
from accountd and can show:

- account alias and active state;
- credential-health status;
- remaining five-hour quota;
- remaining weekly quota;
- time until the five-hour reset.

Install the MOTD adapter after CodexMarathon is installed:

```bash
sudo install -Dm0755 scripts/armbian-motd-codexmarathon.sh \
  /etc/update-motd.d/27-codexmarathon
```

If an older installation also has `42-codexmarathon`, disable or remove that
second hook to avoid rendering the account table twice.

## Shared custom Codex home

The CLI, installer, and accountd now use the same Codex-home resolver. The
resolution order is:

1. `CODEXMARATHON_CODEX_HOME`;
2. `CODEX_HOME`;
3. the persisted installer choice;
4. `$HOME/.codex`.

The installer atomically records the selected path in
`$XDG_CONFIG_HOME/codexmarathon/config.json`, falling back to
`$HOME/.config/codexmarathon/config.json`. The directory is mode `0700` and the
file is mode `0600`. A generated systemd drop-in gives accountd access only to
the selected Marathon metadata directory while explicitly hiding the active
credential file and credential vault.

Conflicting environment overrides, unsafe paths, symbolic-link configuration
directories, and invalid systemd path values are rejected.

## Native multi-account controls

Marathon account operations remain integrated into the modified Codex CLI:

```bash
codex marathon status
codex marathon accounts
codex marathon login work --device-code
codex marathon import personal
codex marathon switch work
codex marathon on
codex marathon off
```

Switches use the native AuthManager path and are accepted only at a safe turn
boundary. Account-bound transports are invalidated after a committed identity
change, and failed or uncertain transitions remain visible for reconciliation.

Quota observations are associated with the identity active for the provider
event. They are persisted asynchronously so a temporary local database failure
cannot delay or fail a Codex turn.

## Encrypted selective account backup

Saved Marathon accounts can now be exported into one password-encrypted file:

```bash
codex marathon backup export --output accounts.cmbackup
```

Interactive export provides a checkbox picker:

- Up/Down moves the cursor.
- Space toggles an account.
- `a` selects or clears all accounts.
- Enter confirms the selection.
- Escape cancels.

Automation can select stable account IDs explicitly or export all accounts:

```bash
codex marathon backup export --output accounts.cmbackup \
  --account ACCOUNT_ID_1 --account ACCOUNT_ID_2
codex marathon backup export --output accounts.cmbackup --all \
  --passphrase-file /secure/codexmarathon-passphrase
```

The encrypted archive contains selected aliases, stable account IDs, and their
opaque credential snapshots. It excludes quota history, reset state, scheduler
jobs, machine paths, the enabled setting, and the active-account marker.
Passphrases are read with terminal echo disabled or from a strictly validated
owner-only file; they are never accepted as command arguments or environment
variables.

## Transactional account restore

Backups can be inspected and restored through the CLI:

```bash
codex marathon backup import accounts.cmbackup --dry-run
codex marathon backup import accounts.cmbackup
```

Import provides three conflict policies:

- `skip` leaves existing IDs or colliding aliases unchanged;
- `replace` updates only the same stable account ID and cannot replace the
  currently active destination account;
- `rename` preserves an imported account while assigning a non-conflicting
  alias.

Import decrypts and validates the complete bounded archive before writing. It
then takes the shared Marathon lock, recalculates the plan, stages fresh
identity-bound vault references, flushes them, and publishes the whole registry
in one atomic commit. The destination `auth.json`, active identity, and enabled
setting are preserved. Imported accounts remain inactive until the user
explicitly switches to one.

## Registry and credential-state hardening

The account registry now supports schema version 2 for transactional imports
and fresh credential references. Version-1 registries remain readable and are
upgraded on their next mutation. Older CodexMarathon versions reject a v2
registry instead of silently misreading it.

Additional safeguards include:

- a process-shared state lock for account mutations;
- orphan snapshot collection before new credential references are allocated;
- atomic writes plus file and directory synchronization;
- identity validation between account metadata and credential snapshots;
- archive count, plaintext, ciphertext, snapshot-size, and scrypt-work bounds;
- wrong-password, truncation, tampering, duplicate-field, and unsupported-format
  rejection before destination state changes;
- an app-server checkpoint of the live active identity before exporting it.

## Automatic-reset foundation

The durable scheduler, events, job state machine, quota observations, and typed
client form the foundation for future auto-spin and system-notification
features. The CLI includes explicit reset controls:

```bash
codex marathon auto-reset status
codex marathon auto-reset on
codex marathon auto-reset off
```

Production provider reset execution is **not implemented or enabled** in this
release. Accountd remains credential-free and cannot send a Codex message or
perform a provider action. A future constrained native executor must own those
side effects, enforce an allow-listed versioned contract, apply idempotency and
rate limits, and return bounded results to accountd. Development paths use
mocked reset capabilities only.

## Documentation

- [Feature and command guide](docs/features-and-usage.md)
- [Account daemon architecture and operations](docs/accountd.md)
- [Encrypted backup and restore](docs/account-backup.md)
- [Account manager behavior](docs/account-manager.md)
- [Release validation](docs/release.md)
- [Runtime provenance](runtime/PROVENANCE.md)
