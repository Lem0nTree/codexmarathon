# CodexMarathon features and usage

CodexMarathon is built into the custom Codex Rust CLI. The release also ships
the private, automatically managed `codexmarathon-accountd` metadata service;
users do not install a plugin, SDK, or separate backup utility.

## Native commands

Use Marathon from a shell:

```bash
codex marathon status
codex marathon accounts
codex marathon login personal --device-code
codex marathon login work --device-code
codex marathon on
codex marathon switch work
codex marathon backup export --output accounts.cmbackup
```

Native `status` and `accounts` show aliases, IDs, enabled state, and transition
state. `accounts --daemon` adds the credential-health and quota summaries from
accountd. Credential snapshots are never printed. `import` stores the identity
that is already active in Codex:

```bash
codex marathon import personal
```

Use `codex marathon off` to leave the profiles stored while disabling
automatic Marathon account management. A switch is accepted only at a safe
turn boundary and updates Codex's native in-memory authentication after the
durable transition is committed.

## Encrypted backup and restore

Export selected saved profiles with the interactive checkbox picker:

```bash
codex marathon backup export --output accounts.cmbackup
```

For a non-interactive job, account selection and an owner-only passphrase file
must be explicit:

```bash
codex marathon backup export --output accounts.cmbackup --all \
  --passphrase-file /secure/codexmarathon-passphrase
codex marathon backup import accounts.cmbackup --dry-run \
  --passphrase-file /secure/codexmarathon-passphrase
codex marathon backup import accounts.cmbackup --yes \
  --passphrase-file /secure/codexmarathon-passphrase
```

The import conflict policy is `skip` by default. Choose `--conflict replace`
to update only matching stable account IDs, or `--conflict rename` to retain an
unrelated destination account and rename the imported alias. The active
identity cannot be replaced. Imported accounts stay inactive until an explicit
`codex marathon switch`.

See [Encrypted account backup and restore](account-backup.md) for archive
limits, password-file rules, atomicity, custom-home behavior, and recovery.

The automatic reset control is explicit and disabled by default:

```bash
codex marathon auto-reset status
codex marathon auto-reset on
codex marathon auto-reset off
```

It is eligible only when every managed account has zero weekly quota and an
account advertises a provider-supported reset capability. Development and test
paths for this exhaustion-based policy use mocks only.

Banked resets have a separate expiry policy:

```bash
codex marathon auto-reset-expiry status
codex marathon auto-reset-expiry on
codex marathon auto-reset-expiry off
```

This policy is off by default. With Marathon enabled, it attempts to redeem
an available banked reset 15 minutes before the provider's expiration time,
even if quota remains. It retries failures until redemption, expiration, or
disablement, preserving the same redemption key across retries and restarts.
Credits without a known expiration are not scheduled. Unknown or incomplete
inventory does not count as proof that a credit was consumed.

The account daemon owns the setting and durable jobs. The separately installed
`codexmarathon-reset-executor` service uses native authentication to refresh
credit inventory and redeem a selected credit without changing the active
account. Both services must be running for unattended operation; a stopped
machine cannot redeem a credit. Turning the policy off prevents new attempts,
but an already submitted provider request may finish. The older exhaustion
policy remains independent and mock-only.

Integrations use `marathon/autoResetExpiry/status` and
`marathon/autoResetExpiry/set` with `{ "enabled": true }` or `{ "enabled": false }`.
Status reports configured and effective state, executor availability, and
per-credit scheduling, retry, and diagnostic metadata. At most 1,000 jobs are
returned; `jobsTruncated` indicates more history, while `nextAttemptAt` covers
all pending jobs. The commands reject
a daemon serving a different Codex home. The same controls are available as
`/marathon auto-reset-expiry status|on|off` in the TUI.

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
/marathon export
```

The login panel asks whether to use a browser link or a device/auth code. On a
headless server, choose device code, open the displayed verification URL on
another machine, and enter the one-time code. After native login completes,
the identity is saved under the requested alias.

`/marathon export` opens a native encrypted-backup wizard. Use Space to toggle
accounts in the checkbox list, then confirm the destination filename and enter
the masked password twice. Existing destination files are not overwritten.
The password remains inside the local TUI and is never included in an app event,
RPC, transcript, or accountd request.

The `/marathon` help and status views use the normal Codex terminal styling:
colored service/health states, bold headings, and an aligned account table with
credential and weekly-quota status.

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
