# Native CodexMarathon work areas

These work areas describe the Rust integration inside the custom Codex CLI.
All changes must preserve Codex's native ownership of authentication, active
turns, transports, conversation state, and recovery.

## Runtime domain

Implement account metadata, opaque credential snapshots, atomic persistence,
transition journals, quota policy, and mock-only automatic reset behavior.

## App-server service

Expose typed Marathon status, enabled-state, auto-reset, import, login, and
switch requests. Verify the source identity and safe-boundary state before
writing or reloading credentials.

## CLI and TUI

Expose `codex marathon` and `/marathon` with matching account controls. Login
must offer a browser-link flow and a device/auth-code flow for headless
servers. The status line must report Marathon enabled state and managed-account
count.

## Verification

Run focused Rust tests and compile checks for the runtime, login, app-server,
TUI, and CLI. Build the release `codex` executable and smoke-test `marathon
--help` and `marathon status`. Never consume a real provider reset while
testing automatic reset.
