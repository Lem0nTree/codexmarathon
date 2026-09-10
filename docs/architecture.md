# Native CodexMarathon architecture

CodexMarathon is a native feature of the custom Codex Rust CLI. The Marathon
command, interactive `/marathon` controls, status line, account service, and
Codex authentication manager all run in the same executable.

```text
codex executable
  |
  +-- `codex marathon` commands
  +-- `/marathon` interactive controls
  +-- status-line Marathon items
  |
  `-- app-server Marathon service
        |-- account registry and opaque credential vault
        |-- quota policy and mock-only reset scheduler
        |-- safe-boundary transition coordinator
        `-- native AuthManager and recovery authorities
```

The CLI starts a typed in-process app-server client. Requests use the same
app-server protocol as the rest of Codex, so account state and transitions do
not depend on a second process, an environment-variable socket, or a separate
authentication implementation.

## Ownership

| Concern | Native Marathon service | Codex core |
| --- | --- | --- |
| Account aliases and profile metadata | owns | observes target |
| Opaque credential snapshots | owns protected storage | provides active snapshot |
| Account policy and transition intent | owns | receives request |
| Running-turn count and safe boundary | consumes | authoritative |
| Auth reload and transport invalidation | requests | performs |
| Conversation and thread state | observes identity | owns |
| Recovery continuation | coordinates release condition | owns parked turn |
| Quota observations and reset policy | ranks and schedules | supplies native observations |

Marathon never changes credentials during an active turn. It validates the
source identity immediately before a transition, atomically persists the
target snapshot, asks Codex to reload its native authentication, invalidates
account-bound transports, and verifies the target identity before committing.

## Runtime crates

`runtime/codex-rs/codexmarathon-runtime` contains the domain model, durable
account state, opaque snapshot vault, transition journal, quota policy, and
native authority bridge. The adjacent `runtime/codexmarathon-adapter` crate
contains reusable typed runtime and recovery bridge types used by the native
app-server composition. It does not own credentials or start another Codex
process.

The app-server protocol defines the typed `marathon/status`,
`marathon/enabled/set`, `marathon/autoReset/set`, `marathon/import`, and
`marathon/switch` requests. Login continues through Codex's native login
service, including the browser-link and device-code flows.

## Recovery and reset rules

Codex owns the parked usage-limit continuation. Marathon releases it only after
the account transition has verified the target identity. If every managed
account has zero weekly quota, the scheduler may consider an account with a
provider reset capability. The capability is disabled by default, and tests
use a fake executor so no real provider reset is called.
