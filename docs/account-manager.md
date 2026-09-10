# Native account manager

The native Marathon account service stores account aliases, quota metadata,
and opaque ChatGPT credential snapshots inside Codex's configured home. It is
part of the app-server process embedded in the Codex executable.

## Account lifecycle

```text
native login or active-account import
             |
             v
validate identity -> write opaque snapshot atomically -> record alias
             |
             v
status/accounts -> safe-boundary switch -> native AuthManager reload
```

Use the public command surface:

```bash
codex marathon import personal
codex marathon login work --device-code
codex marathon accounts
codex marathon switch work
```

The same operations are available from `/marathon` in the interactive TUI.
Login can use a local browser link or device/auth code. Device-code login is
the supported path when the Codex process runs on a headless server.

## Storage rules

- Account metadata contains IDs, aliases, quota observations, and transition
  markers; it never contains token fields.
- Credential snapshots are opaque to the display and logging layers and are
  protected with owner-only file permissions.
- Snapshot writes use a same-directory temporary file, flush, and atomic
  replacement. A failed replacement leaves the previous snapshot intact.
- A switch rechecks the source account and waits for all account-bound work to
  become idle before persisting the target.
- Codex's AuthManager remains the authority for in-memory credentials and
  native provider refresh. Marathon only stores the opaque result needed for a
  later managed switch.

Unsupported authentication modes such as API keys and external bearer
credentials are rejected for managed account switching. Status output,
transition records, and diagnostics contain account IDs and aliases only.
