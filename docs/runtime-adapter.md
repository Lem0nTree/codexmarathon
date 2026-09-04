# Codext runtime adapter seam

This document records the deliberately narrow Rust seam for CodexMarathon
protocol v1. The pinned Codex runtime is imported under `runtime/codex-rs/`;
the adapter and bridge reuse its native authorities rather than creating
parallel authentication, turn, transport, or recovery implementations. The
Go controller only coordinates intent and verifies observed state.

## Boundary and transport

The adapter speaks the shared schemas in `protocol/` over a bidirectional
stream (user-scoped Unix socket, protected named pipe, or an in-process test
pipe). The production Codex app-server creates this listener itself when the
launcher supplies `CODEXMARATHON_LISTEN`; users do not install or launch a
separate adapter process. Messages are
newline-delimited JSON-RPC 2.0. Controller requests use the namespaced methods
defined in `protocol/commands.json`; runtime notifications use the
`codexmarathon/event` method and event types from `protocol/events.json`.

The first request on a connection is `protocol/negotiate`. A runtime must
return a version in the controller's advertised set. A `runtime_ready` event
then announces the runtime ID, selected protocol version, current account
identity, and auth generation. Ordinary state/events contain no credentials;
the three explicitly authenticated native account methods carry opaque
snapshots only in memory over the local protocol and never into the event
journal.

## Observed donor evidence

The read-only checkout under `donor/codext/` is the refresh reference for the
tracked source under `runtime/codex-rs/`. The following symbols are the
evidence for the adapter mapping:

| Marathon operation | Existing Codext owner / evidence | Adapter responsibility |
| --- | --- | --- |
| Active rate-limit snapshot | `codex-rs/app-server-protocol/src/protocol/v2/account.rs`: `GetAccountRateLimitsResponse` (lines 314–325); `common.rs` maps `account/rateLimits/read` (lines 1229–1232) | Call the existing account rate-limit read, narrow it to `RateLimitSnapshotWire`, and emit `rate_limits_snapshot`. |
| Sparse rate-limit update | `account.rs`: `AccountRateLimitsUpdatedNotification` (lines 562–569), documented as a sparse rolling update; `common.rs` maps `account/rateLimits/updated` (line 1902) | Attach the connection's observed account ID and forward as `rate_limits_updated`; the controller merges or refetches when attribution is ambiguous. |
| Runtime identity | `login/src/auth/manager.rs`: `auth_cached` (lines 2328–2334), `auth` (lines 2351–2368), and account accessors around lines 2963–2975 | Report only account ID/auth mode metadata needed for identity verification; do not expose token material. |
| Native account login | `login/src/server.rs`: `run_login_server` and `ServerOptions`; `login/src/auth/manager.rs`: `load_auth_dot_json` | `CodexNativeRuntime` stages the browser callback in a temporary `CODEX_HOME`, then returns an opaque `auth_json` snapshot to the protected controller vault. |
| Native account refresh | `login/src/auth/manager.rs`: `AuthManager::shared` and `refresh_token_from_authority` | `CodexNativeRuntime` stages the supplied snapshot in a temporary `CODEX_HOME`, invokes the existing native refresh authority, and returns only changed token fields; it never shells out to `codex login`. |
| Safe turn boundary | `app-server/src/request_processors.rs`: `reload_auth_from_storage_if_idle` (lines 540–579) checks `subscribe_running_turn_count()` and returns while nonzero; `thread_processor.rs` serializes `thread/start` and `thread/resume` with `auth_transition_lock` (lines 1119–1129 and 3593–3604) | Keep the pending transition ID in adapter state, wait for this existing guard to reach zero, then emit one `safe_boundary_reached`. Do not add a second turn counter. |
| Auth reload | `request_processors.rs`: `reload_auth_from_storage_if_idle` calls `AuthManager::reload_with_status` (lines 557–562), while `handle_auth_reload_status` handles changed/failed outcomes (lines 581–625) | On commit, call the existing reload path and map its outcome to `auth_reload_started`, `auth_reload_succeeded`, or `auth_reload_failed`. |
| Transport invalidation | `request_processors.rs`: changed auth calls `thread_manager.invalidate_model_transport_caches()` (lines 593–600); `core/src/codex_thread.rs` delegates to the model client (lines 269–279) | Never recreate or duplicate the transport cache. Emit `identity_changed` only after the existing invalidation/refresh path returns. |
| Account update | `request_processors.rs`: changed auth refreshes cloud config and emits `AccountUpdated` (lines 601–620) | Use the resulting observed identity for the Marathon event; `AccountUpdated` remains a Codext notification and is not replaced. |
| Auth file compatibility path | `tui/src/auth_watch.rs`: watches `auth.json`, polls every 200ms, and waits through a 200ms quiet period before `AuthFileChanged` (lines 12–24 and 50–77) | Leave the watcher unchanged. Explicit Marathon IPC is primary; the existing watcher remains a compatibility/fallback path. |
| Runtime-owned recovery | `login/src/auth/manager.rs`: `UnauthorizedRecovery` documents reload-then-refresh ordering (lines 1850–1862); `core/src/client.rs` emits recovery events around `handle_unauthorized` (lines 2402–2479) | Forward `recovery_parked`, `recovery_started`, and `recovery_completed` observations. The controller must not construct a recovery prompt or queue. |

The Codex-switch donor supplies the control-plane pattern that motivated this
split: `internal/profile/profile.go` stores independent profile snapshots,
`internal/switcher/switcher.go` performs a temp-write/rename replacement, and
`internal/watcher/watcher.go` confirms an active profile before switching.
Those features are imported into the controller in later account-manager
gates. The runtime adapter must not duplicate them.

## Protocol-to-Codext mapping

| Protocol v1 message | Codext action / result |
| --- | --- |
| `GetRuntimeState` | Read current AuthManager identity, the existing running-turn count, and adapter pending-transition state. |
| `PrepareAuthTransition` | Validate `transition_id`, `target_account_id`, and `expected_generation`; record intent. No auth reload occurs. |
| `SafeBoundaryReached` | Adapter observes the existing running-turn guard at zero. This is a notification, not permission to invent a new boundary authority. |
| `CommitAuthTransition` | Require the exact pending ID/generation, then invoke the existing AuthManager reload path after controller atomic deployment. Reload and `ThreadManager::invalidate_model_transport_caches` execute under the shared auth-transition lock; the result is rejected unless AuthManager observes the exact target account. |
| `account/login` / `account/refresh` | Delegate to `CodexNativeRuntime`'s native login/refresh methods; opaque credentials are not interpreted by the adapter and are never journalled. |
| `account/authSnapshot/read` | Read the active opaque snapshot from the shared native `AuthManager`; the controller synchronizes it into the protected Account A vault before deploying Account B. |
| `CancelAuthTransition` | Clear matching adapter intent without touching AuthManager or conversation state. |
| `GetIdentity` / `GetAuthGeneration` | Return the same runtime-observed identity/generation used in reconciliation. |
| `IdentityChanged` | Report the post-reload account and generation after Codext invalidates account-bound transports and refreshes account state. |

Every transition event carries `transition_id`, `runtime_id`, and
`auth_generation`. A stale command (lower generation or a non-matching pending
ID) is rejected and must not call AuthManager. The adapter should return a
typed `rejected` result; transport loss after a state-changing operation is
reported to the controller as an uncertain outcome to be reconciled, not as a
guessed failure.

## Patch ledger

The runtime patch should stay reviewable and reappliable onto fresh upstream
Codex versions:

| ID | Narrow addition | Explicitly not changed |
| --- | --- | --- |
| M01 | Marathon JSON-RPC server and version negotiation | Existing app-server transport semantics |
| M02 | In-memory auth-generation counter persisted only in adapter/runtime state | AuthManager token refresh and storage format |
| M03 | Transition ID correlation, pending intent, and stale-generation rejection | Existing `auth_transition_lock` and running-turn guard |
| M04 | Runtime identity/status API and rate-limit event forwarding | Existing rate-limit provider and account read implementation |
| M05 | Safe-boundary, reload, identity, and recovery event translation | Existing transport invalidation, recovery queue, and conversation/session manager |

The MVP does not require controller-owned repository checkpointing, a second
active-account quota poller, a custom `AuthManager`, custom transport teardown,
or a controller recovery prompt. These were explicitly removed by the
architecture freeze because they would create competing authorities or
duplicate Codext behavior.

## Adapter acceptance checks

Before the adapter is considered complete, its focused tests should prove:

1. A commit requested while a turn is running remains pending until Codext's
   running-turn count reaches zero.
2. A stale generation or transition ID is rejected without reloading auth.
3. Auth reload failure emits `auth_reload_failed` and leaves the prior identity
   authoritative.
4. A changed auth reload uses Codext's existing model-transport invalidation
   path before `identity_changed`, and a reload for the wrong account is
   rejected with `identity_mismatch`.
5. An active `account/authSnapshot/read` result is synchronized to Account A's
   protected vault before the target deployment, and a mismatched snapshot
   identity leaves the transition unresolved.
6. A sparse rate-limit notification retains the runtime connection identity
   and requests a full read when multiple by-ID buckets make attribution
   ambiguous.
7. Runtime recovery events are observed exactly once; no controller-generated
   continuation is added.
