# Build and verification

The single repeatable entry point is `verify.ps1` at the repository root:

```powershell
pwsh -NoProfile -File .\verify.ps1
```

It validates repository files and JSON protocol documents on every host. When
available it also runs:

* `go test ./...` in `controller/`;
* `go test ./...` in `integration/`;
* `cargo test` in `runtime/codexmarathon-adapter/`; and
* `cargo test -p codexmarathon-runtime -p codex-cli` from
  `runtime/codex-rs/`.

Each missing executable is printed as `BLOCKED: <tool> not found`, with an
install/toolchain note. A static failure exits `1`; missing optional
toolchains exit `2` after all other checks have run. This distinguishes an
observed pass from a check that could not be executed.

The runtime baseline is embedded and has no separate Codext process
dependency. The controller now includes the supervised launcher, local IPC
policy, account/profile commands, telemetry policy loop, and durable recovery
domains. The deterministic fake runtime still proves controller behavior, but
it is not a substitute for the compiled embedded-runtime smoke or live
provider acceptance. Use [docs/release.md](release.md) for the evidence labels,
packaging checks, and clean-machine validation commands.
