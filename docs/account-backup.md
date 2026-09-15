# Encrypted account backup and restore

CodexMarathon can export selected saved account profiles to one encrypted file
and import them into another CodexMarathon installation. The implementation is
compiled into the modified `codex` binary through the internal
`codexmarathon-transfer` crate. It is not a plugin, separate package, daemon,
or network API.

`codexmarathon-accountd` remains a credential-free metadata and quota service.
It does not read, encrypt, decrypt, transmit, or import account credentials.

## Interactive workflow

Create a backup:

```bash
codex marathon backup export --output accounts.cmbackup
```

Or launch the integrated wizard from a running Codex session:

```text
/marathon export
```

The TUI wizard collects the account selection, output filename, password, and
password confirmation locally. Secret input is masked and zeroized and never
crosses the app-server or accountd protocols.

The picker uses Up/Down to move, Space to toggle an account, `a` to select all,
Enter to continue, and Escape to cancel. Export prompts for the passphrase
twice with terminal echo disabled. A passphrase must contain at least 12
Unicode characters and at most 1024 UTF-8 bytes. Its exact whitespace and
Unicode are significant; there is no password-recovery mechanism.

Preview and then apply an import:

```bash
codex marathon backup import accounts.cmbackup --dry-run
codex marathon backup import accounts.cmbackup
```

The preview decrypts and validates the whole archive but does not create or
change destination state. Interactive import shows the planned imported,
replaced, and skipped profiles before confirmation. Because the archive is
revalidated and the plan is recalculated under the write lock, a later state
change cannot silently reuse a stale preview.

## Explicit and unattended operation

Repeat `--account` with stable account IDs, or use `--all`:

```bash
codex marathon backup export --output accounts.cmbackup \
  --account ACCOUNT_ID_1 --account ACCOUNT_ID_2
```

When standard input or error is not a terminal, export requires `--account` or
`--all` and both operations require `--passphrase-file`. A real import also
requires `--yes`:

```bash
chmod 600 /secure/codexmarathon-passphrase
codex marathon backup export --output accounts.cmbackup --all \
  --passphrase-file /secure/codexmarathon-passphrase
codex marathon backup import accounts.cmbackup --dry-run \
  --passphrase-file /secure/codexmarathon-passphrase
codex marathon backup import accounts.cmbackup --yes \
  --passphrase-file /secure/codexmarathon-passphrase
```

The passphrase file must be a regular, non-symlink file owned by the current
user with no group or other permission bits. CodexMarathon removes exactly one
terminal LF or CRLF and rejects any remaining newline, carriage return, NUL,
oversized value, or unsafe path. A passphrase is intentionally never accepted
through a command argument or environment variable because those channels are
commonly exposed by process inspection and diagnostics.

## Conflict policies

Use `--conflict` on import:

- `skip` is the default. An existing stable account ID or colliding alias is
  left unchanged.
- `replace` updates only the profile with the same stable account ID. It never
  replaces a different ID that happens to use the same alias, and it rejects
  replacement of the active destination identity.
- `rename` preserves IDs and snapshots but assigns a numbered alias when an
  imported alias collides with an unrelated destination profile.

Imported profiles are inactive. Import preserves the destination's active
account, enabled setting, active native credentials, and `auth.json`. Use
`codex marathon switch <alias-or-id>` after import to install one imported
snapshot through the normal native AuthManager path.

## Archive and transaction security

The `.cmbackup` suffix is conventional; the file name is not security
significant. The archive is an authenticated age passphrase envelope using a
fixed, bounded scrypt work factor. Its encrypted version-1 manifest contains:

- the format and version markers;
- creation time and exporting CodexMarathon version;
- each selected stable account ID, alias, and exact opaque credential snapshot.

It excludes the source state path, active marker, enabled setting, quota and
reset observations, health state, events, scheduler state, and machine
metadata. A maximum of 256 profiles, 1 MiB per decoded snapshot, 16 MiB of
decrypted JSON, and a bounded ciphertext/header size protect import from
untrusted resource consumption. Identity-less or mismatched snapshots,
unknown fields, unsupported versions, duplicate IDs or aliases, excessive
scrypt work, truncation, tampering, and a wrong passphrase are rejected before
any destination write.

Export writes a mode-0600 temporary ciphertext file, flushes it, and atomically
publishes it. Existing output is not replaced unless `--overwrite` is present,
and native `auth.json` or Marathon state paths cannot be export destinations.
No plaintext export file is created.

Import takes the shared Marathon process lock, reloads and replans against the
authoritative registry, writes each snapshot under a fresh identity-bound vault
reference, flushes it, and then makes one atomic full-registry commit. The
registry commit is the visibility point for the batch. A crash before it can
leave only unreachable staged snapshots, which a later safe write can collect;
a crash after it exposes the complete batch. An uncertain commit is reported
for inspection instead of being guessed successful.

When an export includes the current profile, the CLI asks native AuthManager to
checkpoint that identity and authentication generation first. Only account ID
and generation cross that internal request; credential bytes remain inside the
native service and vault.

## State directory behavior

The release installer persists the chosen Codex state root at:

```text
$XDG_CONFIG_HOME/codexmarathon/config.json
```

If `XDG_CONFIG_HOME` is unset, it uses
`$HOME/.config/codexmarathon/config.json`. The directory is mode 0700 and the
file is mode 0600. Resolution order is:

1. `CODEXMARATHON_CODEX_HOME`;
2. `CODEX_HOME`;
3. the persisted installer choice;
4. `$HOME/.codex`.

If both environment variables are set, they must identify the same normalized
path. The installed CLI and accountd share this resolver, while an explicit
daemon `--codex-home` remains authoritative. Backups contain no source path and
are restored into the destination's resolved Marathon vault; no file is copied
directly to `auth.json`.

## Compatibility and recovery

The transaction-safe vault references use Marathon registry schema version 2.
This release reads existing version-1 registries and upgrades them on the next
mutation. Older CodexMarathon binaries reject a version-2 registry, so stop old
CLI processes before upgrading and do not downgrade after changing account
state without restoring a complete compatible state backup.

Keep at least one tested encrypted backup somewhere separate from the source
machine, and store its passphrase separately. Before disaster recovery, run a
dry run on the destination, review conflicts, apply the import, list the saved
profiles, and switch explicitly to the intended account. A lost passphrase
cannot be reset or recovered by CodexMarathon.
