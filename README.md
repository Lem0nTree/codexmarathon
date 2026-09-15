# CodexMarathon

CodexMarathon adds multi-account Marathon controls directly to a custom build
of the Codex CLI. Its release installer also installs and starts the private
`codexmarathon-accountd` user service automatically; users do not need to run
a controller command or configure a Marathon socket environment variable.

It stores managed account profiles, lets you sign in to more than one ChatGPT
account, switches only at a safe boundary between turns, and can move selected
profiles between CodexMarathon installations in a password-encrypted backup.
Credential snapshots are treated as opaque, kept in owner-only local state,
and never appear in status output, logs, or transition records.

## Install

CodexMarathon is distributed as one package: the modified `codex` CLI, the
private account metadata daemon, and its systemd user units are installed and
started together.

```bash
curl -fsSL https://raw.githubusercontent.com/Lem0nTree/codexmarathon/main/scripts/install.sh | sh
```

No separate daemon, SDK, or plugin installation is required. The installer
enables the user service and socket, enables user lingering for operation after
logout, and verifies the account API before reporting success.

To use a non-default Codex state directory, pass an absolute path:

```bash
curl -fsSL https://raw.githubusercontent.com/Lem0nTree/codexmarathon/main/scripts/install.sh \
  | CODEXMARATHON_CODEX_HOME=/srv/codex-state sh
```

The installer records that selection in the owner-private
`$XDG_CONFIG_HOME/codexmarathon/config.json` (or
`$HOME/.config/codexmarathon/config.json`). The modified CLI and accountd use
it automatically on later invocations, so shell startup changes are not
required. An explicit `CODEXMARATHON_CODEX_HOME` or `CODEX_HOME` still takes
precedence; setting both to different paths is rejected. The default remains
`$HOME/.codex`. The local API socket always stays under `$XDG_RUNTIME_DIR` and
does not move with the state directory.

## Native workflow

Use Marathon from the shell:

```bash
codex marathon status
codex marathon login personal --device-code
codex marathon login work --device-code
codex marathon on
codex marathon switch work
```

Or use it from an interactive Codex session:

```text
/marathon
/marathon login personal
/marathon login work
/marathon on
/marathon switch work
```

Running `/marathon` displays the current native state and a help panel. It
also explains the available account, login, enabled-state, and switching
commands.

## Sign in on a server

`login` supports a browser-link flow and a device-code flow.

For a headless machine such as an AWS instance, use a device code:

```bash
codex marathon login work --device-code
```

Codex prints a verification URL and one-time code. Open the URL on your own
machine, enter the code, and complete the ChatGPT sign-in. Once Codex receives
the native login completion, it stores that identity under the `work` alias.

In an interactive Codex session, use:

```text
/marathon login work
```

Codex presents **Browser link** and **Device / auth code** choices. You can
also choose explicitly:

```text
/marathon login work browser
/marathon login work device-code
```

For a local browser flow from the shell, omit the flag and choose the prompt,
or pass `--browser` explicitly.

## Commands

```text
codex marathon status
codex marathon accounts
codex marathon on
codex marathon off
codex marathon import <alias>
codex marathon login <alias> [--browser | --device-code]
codex marathon switch <alias-or-id>
codex marathon backup export --output <file> [--account <id> ... | --all] [--overwrite]
codex marathon backup import <file> [--conflict skip|replace|rename] [--dry-run | --yes]
```

`import` saves the identity that is already active in Codex. `login` uses the
normal native Codex ChatGPT login flow and imports the completed account under
the alias automatically. `switch` resolves an alias or account ID and changes
the active identity only when no turn is running.

The matching interactive commands are:

```text
/marathon
/marathon status
/marathon on
/marathon off
/marathon import <alias>
/marathon login <alias> [browser|device-code]
/marathon switch <alias-or-id>
/marathon export
```

Configure the status line with the `marathon` and `marathon-accounts` items.
They display whether native Marathon is enabled and the number of managed
accounts.

## Encrypted account backup

Backups operate on saved Marathon profiles, not merely the currently installed
`auth.json`. In an interactive terminal, omit the account flags to get a
checkbox picker:

```bash
codex marathon backup export --output accounts.cmbackup
codex marathon backup import accounts.cmbackup --dry-run
codex marathon backup import accounts.cmbackup
```

Inside the Codex TUI, `/marathon export` opens the same native workflow: select
accounts with checkboxes, choose the destination filename, and enter and
confirm a masked backup password. The password stays in zeroizing local TUI
state; it is never sent to accountd, app-server RPCs, session history, or debug
events.

Export asks for the passphrase twice and import asks once; terminal echo stays
disabled while it is entered. For unattended use, pass an owner-only file with
`--passphrase-file` and select accounts explicitly with repeatable `--account`
or `--all`. Passphrases are never accepted in command arguments or environment
variables.

The archive is authenticated age encryption with a bounded scrypt work factor.
Import validates and decrypts the complete archive before changing state,
stages credentials under fresh vault references, and commits the registry as
one atomic update. Imported accounts remain inactive, and import never replaces
the destination's active `auth.json`. Conflicts default to `skip`; `replace`
cannot replace the active identity, while `rename` changes only a colliding
alias. See [Encrypted account backup and restore](docs/account-backup.md) for
the full workflow and security contract.

## Local account API

`codexmarathon-accountd` provides a versioned, read-only API for bundled or
custom local applications. It exposes account aliases, active state,
credential health, five-hour and weekly quota windows, exact reset times,
observation freshness, scheduler status, and durable metadata events. It does
not expose authentication credentials and does not listen on TCP.

The owner-only Unix socket is:

```text
$XDG_RUNTIME_DIR/codexmarathon-accountd/accountd.sock
```

The wire format is newline-delimited JSON. Protocol version 1 supports:

- `health` — service and protocol readiness
- `status` — database, scheduler, event cursor, and job counts
- `accounts` — typed account and quota snapshots
- `events_since` — bounded event replay after a durable cursor

For normal shell use, the CLI is the simplest API client:

```bash
codex marathon accounts --daemon --format table
codex marathon accounts --daemon --format json
```

Rust components in this workspace use the private
`codexmarathon-accountd-client` crate. It provides typed `health()`,
`status()`, `accounts()`, and `events_since()` calls with request-ID and
protocol-version validation, bounded responses, and deadlines. The crate is
compiled into shipped applications; users do not install it separately.

A raw health request looks like:

```json
{"version":1,"id":1,"method":"health","params":null}
```

Applications should use the typed client when possible and treat accountd as
the single metadata owner. They must not read its SQLite database directly or
place credentials, authorization headers, cookies, or arbitrary commands in
requests.

## Safety model

- Only managed ChatGPT authentication profiles are switchable initially.
- A switch waits until all account-bound work is idle; it never changes an
  active model response, tool call, or subagent turn.
- The source identity is checked again before a transition. The target profile
  is validated and persisted atomically before Codex updates its in-memory
  credentials.
- A failed or uncertain transition is recorded as such and requires
  reconciliation; Marathon never guesses that a credential change succeeded.
- API keys, external bearer authentication, workload identity, and other
  unsupported provider modes are rejected for Marathon switching.
- Account backups are encrypted before publication and contain no quota,
  scheduler, event, active-account, source-path, or machine metadata.
- `codexmarathon-accountd` remains credential-free; backup and restore run
  inside the CLI against the native Marathon vault.

## Quota reset actions

The native runtime includes a mock-tested capability and scheduler for a
provider-supported quota-reset action. Its intended behavior is to use a real
provider reset action only after every eligible managed account has reached
zero weekly quota, then verify the refreshed quota before selecting that
account.

No real reset action is called by this project during development, tests, or
the current build. Production execution remains disabled until a provider
integration exposes a documented reset capability and is explicitly enabled.

## Build

The custom CLI is built from the Codex Rust workspace:

```bash
cd runtime/codex-rs
cargo build --release -p codex-cli -p codexmarathon-accountd
```

The resulting executable is:

```text
runtime/codex-rs/target/release/codex
```

Run focused checks while developing:

```bash
cargo check -p codexmarathon-runtime
cargo check -p codex-login
cargo check -p codex-app-server
cargo check -p codex-tui
cargo check -p codex-cli
```

## Documentation

- [Features and usage](docs/features-and-usage.md)
- [Encrypted account backup and restore](docs/account-backup.md)
- [Architecture](docs/architecture.md)
- [Build and verification](docs/build-test.md)
- [Runtime provenance](runtime/PROVENANCE.md)
