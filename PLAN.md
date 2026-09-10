# Native CodexMarathon plan

## Product outcome

Ship a custom Codex Rust CLI with Marathon account management built into the
same executable. Users manage accounts with `codex marathon` or `/marathon`
without a companion process, external socket, or second authentication tool.

The native integration must provide:

1. Secret-free account aliases, status, import, login, and switching.
2. Browser-link and device-code login for local and headless machines.
3. Safe-boundary transitions through Codex's AuthManager and transport
   invalidation authorities.
4. Status-line indicators for Marathon enabled state and account count.
5. Quota-aware account selection and durable transition/recovery evidence.
6. Opt-in automatic reset policy with mock-only development/test executors.

## Runtime ownership

Codex owns authentication authority, active-turn state, model transports,
conversation state, and parked recovery continuation. Marathon owns aliases,
opaque snapshots, quota policy, transition intent, and reconciliation metadata.
The source identity is verified before every transition; target credentials are
written atomically and reloaded only after Codex reaches a safe boundary.

## Verification gates

- Focused Rust tests cover account storage, auth transitions, quota policy,
  recovery ordering, and automatic-reset idempotence.
- `cargo check` covers the Marathon runtime, login, app-server, TUI, and CLI.
- A release build produces one `codex` executable containing the native
  `marathon` command and interactive `/marathon` controls.
- Live acceptance uses a disposable Codex home and never invokes a real quota
  reset while the provider integration is disabled.
