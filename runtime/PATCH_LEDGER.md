# Runtime patch ledger

This ledger records the local runtime integration over the pinned Codext
workspace. The donor checkout remains read-only and is not a Cargo dependency;
the product builds from the tracked copy under `runtime/codex-rs`.

| ID | Implemented in this patch | Deliberately delegated to Codext |
| --- | --- | --- |
| M01 | `JsonLineCodec`, JSON-RPC request/response/notification types, first-request version negotiation, and `runtime_ready` | Existing app-server transport and connection lifecycle |
| M02 | Monotonic in-memory Marathon auth generation incremented only after a native changed reload | AuthManager token refresh, auth storage format, and persistence across runtime restart |
| M03 | Pending transition ID/target/generation checks, typed rejected results, and one safe-boundary event at authoritative count zero | Existing `auth_transition_lock` and running-turn guard |
| M04 | Runtime identity/status result types and full/sparse rate-limit event forwarding with connection account identity | Existing account rate-limit provider/read implementation and controller-side sparse attribution |
| M05 | Auth reload/success/failure, post-invalidation identity change, turn, and deduplicated recovery event translation | Existing AuthManager reload, model-transport invalidation/config refresh, recovery queue, and conversation/session manager |
| M06 | Imported pinned Codex Rust workspace, `codexmarathon-runtime` bridge, and `codex-cli::codexmarathon` re-export | Later feature agents wire the bridge hooks to the concrete multi-account policy and controller lifecycle |
| M07 | Native `CodexNativeRuntime`: shared AuthManager/turn-watch/auth-transition-lock composition, isolated native login/refresh handlers, active opaque snapshot read for Account A synchronization, atomic reload-plus-transport invalidation, and exact target-identity validation | Account rate-limit reads still come from the native app-server account processor; the listener/launcher remains Agent 6 work |

## Integration limitations

1. The imported runtime is a pinned copy and does not modify `donor/codext`.
   `codexmarathon-runtime::NativeCodexRuntime` is the typed hook for wiring
   the imported Codex authorities without a second runtime process.
2. `CodexNativeRuntime` is a concrete native authority bridge, but the
   Marathon adapter is still a library seam, not a Unix-socket/named-pipe listener. The
   controller-supervisor task still needs to own process startup, stream
   acceptance, and calling `handle_frame`/`drain_event_lines`.
3. Safe-boundary observation is callback/poll based. The adapter never starts
   a second turn counter or background watcher; Codext must call
   `observe_turn_count` from its existing authoritative watch.
4. The generation counter is intentionally runtime-local. On restart the
   integration must seed `AdapterConfig::initial_auth_generation` from the
   controller's reconciliation state; the adapter does not persist auth or
   infer a generation from disk.
5. A sparse rate-limit update is only tagged with the connection's observed
   account. The Go controller remains responsible for merge/refetch and
   ambiguous multi-bucket attribution.
6. Account A synchronization is wired through the optional native snapshot
   reader and the controller's protected vault writer. A host that does not
   provide that native reader retains legacy compatibility but cannot claim
   refreshed-token synchronization.
