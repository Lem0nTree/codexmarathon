# CodexMarathon embedded runtime

`runtime/codex-rs` is the tracked, pinned Codex Rust runtime imported from
Loongphy/codext. It preserves the normal Codex CLI/TUI behavior and is the
runtime shipped by CodexMarathon; users do not install or launch Codext
separately. See [`PROVENANCE.md`](PROVENANCE.md) for the source commit and
license notices.

`codexmarathon-adapter` remains the narrow protocol seam, and
`codexmarathon-runtime` is the in-process bridge that connects that seam to
native Codex authorities. Both are members of the embedded Cargo workspace.

The crate owns:

- newline-delimited JSON-RPC framing and protocol v1 wire types;
- per-connection protocol negotiation and `runtime_ready`;
- pending `TransitionID` correlation and stale `AuthGeneration` rejection;
- safe-boundary and identity/reload event translation; and
- forwarding of full and sparse rate-limit observations plus runtime-owned
  recovery lifecycle events.

The `NativeCodexRuntime` implementation is the Codex integration point. It
delegates to the existing AuthManager reload path, authoritative running-turn
watch, account rate-limit read, account-bound model-transport invalidation,
and recovery state machine. The adapter does not read credentials, refresh
tokens, create a second turn counter, invalidate transports, or create
recovery prompts.

The adapter is synchronous by design. The bundled app-server starts its
`RuntimeServer` when `CODEXMARATHON_LISTEN` is set by the Go launcher; the
listener uses a user-scoped Unix socket or a local Windows named pipe and
keeps transition intent across reconnects. It periodically reads the native
Codex rate-limit authority and forwards safe snapshots/events to the
controller. No donor path appears in the embedded Cargo workspace.
