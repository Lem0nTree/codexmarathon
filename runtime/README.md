# Optional Codex runtime and local-control adapter

`runtime/codex-rs` is the tracked, pinned Codex-derived source used to develop
and test CodexMarathon's supported local-control protocol. It is retained for
provenance and for an explicitly opt-in self-contained diagnostic package.
The normal CodexMarathon release is a Go companion and uses the user's
separately installed `codex` executable.

`codexmarathon-adapter` is the narrow protocol seam, and
`codexmarathon-runtime` is the in-process bridge to Codex's native AuthManager,
turn state, telemetry, transport invalidation, and recovery authorities. The
adapter does not read the companion vault, refresh tokens independently,
create a second turn counter, invalidate transports itself, or create a
second recovery prompt.

Build this tree only when working on the optional runtime path:

```text
cd runtime/codex-rs
cargo test --locked -p codexmarathon-runtime
cargo build --locked --release -p codex-app-server
```

The resulting app-server is never packaged by default. Use
`--include-embedded-runtime` or `CODEXMARATHON_INCLUDE_EMBEDDED_RUNTIME=1`
only for controlled protocol diagnostics. No donor path appears in the Cargo
workspace.
