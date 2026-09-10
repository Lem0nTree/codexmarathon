# Native runtime bridge

The Rust adapter crate is an internal typed seam used by the embedded
app-server composition. It keeps the native Marathon service independent from
transport details while preserving Codex's ownership of authentication,
turns, transports, and recovery.

`codexmarathon-runtime` supplies the native backend implementation. The
adapter types carry only account IDs, aliases, generation numbers, quota
windows, transition IDs, and recovery outcomes. Credential snapshots remain
opaque and are never logged or serialized into diagnostics.

The app-server's normal in-process client is the supported user-facing path:

```text
codex marathon or /marathon
          |
          v
typed app-server request
          |
          v
native Marathon service -> AuthManager / turn manager / recovery owner
```

The adapter does not spawn a process, own a second turn counter, or implement
provider authentication. Its tests use fake native authorities and fake reset
executors so a test cannot consume a real account reset.
