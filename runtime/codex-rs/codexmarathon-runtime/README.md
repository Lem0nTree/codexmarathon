# Embedded CodexMarathon runtime bridge

This crate is the in-process integration seam between the pinned Codex Rust
runtime and the CodexMarathon protocol adapter. It is part of the embedded
workspace; it is not a replacement runtime and it does not start Codext as a
child process.

Construct [`CodexNativeRuntime`](src/native.rs) from the Codex composition
root when the shared authorities are available, or implement
[`NativeCodexRuntime`](src/lib.rs) for another native host. The concrete bridge
forwards:

- `AuthManager` identity and storage reload;
- the authoritative running-turn count;
- account rate-limit snapshots and sparse updates;
- account-bound model-transport invalidation; and
- Codex's existing parked recovery lifecycle.

`CodexNativeRuntime` also exposes the Marathon `account/login` and
`account/refresh` handlers through Codex's `run_login_server` and
`AuthManager::refresh_token_from_authority`. Login and refresh stage
credentials in an isolated temporary `CODEX_HOME`; only the opaque snapshot
or token result crosses the local adapter boundary. A secondary account
operation never overwrites the active `auth.json`.

The `account/authSnapshot/read` handler reads the current serializable
snapshot from that same shared `AuthManager`. The controller uses it at the
safe boundary to write back refreshed Account A credentials to its protected
vault before deploying Account B; the runtime and adapter never log or
journal the opaque value.

Wrap that implementation in [`EmbeddedRuntime`](src/lib.rs). The existing
adapter then owns only protocol framing, transition correlation, generation
checks, safe-boundary notifications, and event translation. It never
interprets opaque token snapshots or creates a second turn/recovery authority.

The bridge is intentionally synchronous at its native authority boundary.
The production app-server wraps it in `RuntimeServer` when the launcher sets
`CODEXMARATHON_LISTEN`; that listener serves the adapter over a user-scoped
Unix socket or Windows named pipe. The server keeps pending transition intent
across reconnects and resets only per-connection protocol negotiation.
