# Build and verification

The single repeatable entry point is `verify.ps1` at the repository root:

```powershell
pwsh -NoProfile -File .\verify.ps1
```

It validates repository files and JSON protocol documents on every host. When
available it also runs:

* `go test ./...` in `controller/`;
* `go test ./...` in `integration/`;
* `cargo test` in `runtime/codexmarathon-adapter/`.

Each missing executable is printed as `BLOCKED: <tool> not found`, with an
install/toolchain note. A static failure exits `1`; missing optional
toolchains exit `2` after all other checks have run. This distinguishes an
observed pass from a check that could not be executed.

The current MVP has no listener binary in the Rust crate. End-to-end live IPC
requires a Codext patch or a harness that implements the adapter backend and
owns the local endpoint. The in-process fake runtime still proves controller
transition and reconciliation behavior without that external process.

