# Native Codex runtime

`runtime/codex-rs` is the tracked Codex Rust workspace used to build the
custom CLI. Marathon is compiled into the same `codex` executable and uses the
native app-server, AuthManager, turn manager, transport invalidation, and
recovery authorities.

`codexmarathon-runtime` contains the account registry, opaque snapshot vault,
transition journal, quota policy, automatic-reset state, and native authority
bridge. `codexmarathon-adapter` contains the typed internal bridge interfaces
used by the app-server composition. Neither crate starts a second Codex
process or prints credentials.

Build the CLI from the Rust workspace:

```bash
cd runtime/codex-rs
cargo fmt --all -- --check
cargo test --locked -p codexmarathon-runtime
cargo build --locked --release -p codex-cli
```

The resulting executable is `target/release/codex`. It provides:

```text
codex marathon status
codex marathon login work --device-code
codex marathon switch work
```

The interactive TUI provides the matching `/marathon` commands and status-line
items. Automatic quota reset remains disabled by default and is tested with
mocks only.
