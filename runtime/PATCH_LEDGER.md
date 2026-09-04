# Runtime patch ledger

This ledger is the implementation boundary for the Rust adapter. The donor
checkout remains read-only and is not a Cargo dependency.

| ID | Implemented in this patch | Deliberately delegated to Codext |
| --- | --- | --- |
| M01 | `JsonLineCodec`, JSON-RPC request/response/notification types, first-request version negotiation, and `runtime_ready` | Existing app-server transport and connection lifecycle |
| M02 | Monotonic in-memory Marathon auth generation incremented only after a native changed reload | AuthManager token refresh, auth storage format, and persistence across runtime restart |
| M03 | Pending transition ID/target/generation checks, typed rejected results, and one safe-boundary event at authoritative count zero | Existing `auth_transition_lock` and running-turn guard |
| M04 | Runtime identity/status result types and full/sparse rate-limit event forwarding with connection account identity | Existing account rate-limit provider/read implementation and controller-side sparse attribution |
| M05 | Auth reload/success/failure, post-invalidation identity change, turn, and deduplicated recovery event translation | Existing AuthManager reload, model-transport invalidation/config refresh, recovery queue, and conversation/session manager |

## Integration limitations

1. This crate does not modify `donor/codext`; a future Codext patch must
   implement `CodextBackend` and connect its native async event sources.
2. It is a library seam, not a Unix-socket/named-pipe listener. The
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

