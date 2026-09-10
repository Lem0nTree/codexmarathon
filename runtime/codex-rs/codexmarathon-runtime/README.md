# Native Marathon runtime

This crate is the in-process domain and authority bridge for Marathon inside
the custom Codex CLI. It composes with Codex's existing AuthManager, running
turn watch, model-transport invalidation, rate-limit observations, and parked
recovery lifecycle.

The crate owns account aliases, opaque credential snapshots, atomic storage,
transition evidence, quota policy, and mock-only automatic-reset state. It
does not print credentials, create a second turn counter, or start another
Codex executable.

The CLI and TUI call the app-server's typed Marathon requests. Login continues
through Codex's native login service, including browser-link and device-code
flows. A device-code login is suitable for a headless server because the
verification URL and one-time code can be completed on another machine.

Automatic reset is disabled by default. The reset capability and executor are
mocked in tests; no real provider reset action is called by this crate.
