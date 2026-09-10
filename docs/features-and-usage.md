# CodexMarathon features and usage

CodexMarathon is built into the custom Codex Rust CLI. The installed result is
one `codex` executable; there is no companion process, control socket, or
second command to supervise.

## Native commands

Use Marathon from a shell:

```bash
codex marathon status
codex marathon accounts
codex marathon login personal --device-code
codex marathon login work --device-code
codex marathon on
codex marathon switch work
```

`status` and `accounts` show only aliases, IDs, quota summaries, enabled state,
and transition state. Credential snapshots are never printed. `import` stores
the identity that is already active in Codex:

```bash
codex marathon import personal
```

Use `codex marathon off` to leave the profiles stored while disabling
automatic Marathon account management. A switch is accepted only at a safe
turn boundary and updates Codex's native in-memory authentication after the
durable transition is committed.

The automatic reset control is explicit and disabled by default:

```bash
codex marathon auto-reset status
codex marathon auto-reset on
codex marathon auto-reset off
```

It is eligible only when every managed account has zero weekly quota and an
account advertises a provider-supported reset capability. Development and test
paths use mocks only; no provider reset action is invoked by this project.

## Interactive commands

Inside a running Codex session, `/marathon` opens the status and help panel.
The same actions are available as slash commands:

```text
/marathon
/marathon status
/marathon accounts
/marathon on
/marathon off
/marathon auto-reset status
/marathon auto-reset on
/marathon auto-reset off
/marathon import personal
/marathon login work
/marathon login work browser
/marathon login work device-code
/marathon switch work
```

The login panel asks whether to use a browser link or a device/auth code. On a
headless server, choose device code, open the displayed verification URL on
another machine, and enter the one-time code. After native login completes,
the identity is saved under the requested alias.

## Status line

Add the `marathon` and `marathon-accounts` items to the Codex status line. The
first shows whether Marathon is enabled; the second shows the number of
managed accounts.

## Account and transition safety

Only managed ChatGPT profiles can be switched. Codex remains authoritative for
the active turn, authentication reload, account-bound transport invalidation,
and recovery continuation. Marathon coordinates the policy and durable account
metadata, and asks Codex to perform a switch only after all account-bound work
is idle.

The source identity is checked again before deployment. Target credentials are
validated and written atomically. A failed or uncertain transition remains
visible for reconciliation and is never reported as committed merely because a
file was changed.

## Build and verification

The native CLI is built from the tracked Rust workspace:

```bash
cd runtime/codex-rs
cargo build --release -p codex-cli
cargo test --locked -p codexmarathon-runtime
```

See [Build and verification](build-test.md) for the focused checks and
[Runtime provenance](../runtime/PROVENANCE.md) for the imported Codex source.
