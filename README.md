# CodexMarathon

CodexMarathon adds multi-account Marathon controls directly to a custom build
of the Codex CLI. It is a single `codex` executable: users do not need to run
a separate controller or set a Marathon socket environment variable.

It stores managed account profiles, lets you sign in to more than one ChatGPT
account, and switches only at a safe boundary between turns. Credential
snapshots are opaque, protected by Codex's configured credential storage, and
never appear in status output, logs, or transition records.

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
```

Configure the status line with the `marathon` and `marathon-accounts` items.
They display whether native Marathon is enabled and the number of managed
accounts.

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
cargo build --release -p codex-cli
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

## Legacy companion

Earlier repository versions used a separate Go `codexmarathon` controller.
It remains in the source tree for compatibility and migration work, but the
supported user workflow is the native `codex marathon` and `/marathon`
interface described above.

## Documentation

- [Features and usage](docs/features-and-usage.md)
- [Architecture](docs/architecture.md)
- [Build and verification](docs/build-test.md)
- [Runtime provenance](runtime/PROVENANCE.md)
