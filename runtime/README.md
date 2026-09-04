# CodexMarathon runtime adapter

`codexmarathon-adapter` is a standalone Rust crate containing the narrow
runtime-side seam for protocol v1. It is intentionally independent from the
reference checkout in `donor/codext`.

The crate owns:

- newline-delimited JSON-RPC framing and protocol v1 wire types;
- per-connection protocol negotiation and `runtime_ready`;
- pending `TransitionID` correlation and stale `AuthGeneration` rejection;
- safe-boundary and identity/reload event translation; and
- forwarding of full and sparse rate-limit observations plus runtime-owned
  recovery lifecycle events.

The `CodextBackend` implementation is the future Codext patch point. It must
delegate to the existing AuthManager reload path, authoritative running-turn
watch, account rate-limit read, model-transport invalidation, and recovery
state machine. The adapter does not read credentials, refresh tokens, create a
second turn counter, invalidate transports, or create recovery prompts.

The adapter is synchronous by design so it can be compiled and tested without
pulling the donor workspace into this repository. A Codext integration can
bridge its async APIs at the process boundary and call `observe_turn_count`
when the existing turn-watch value changes.

