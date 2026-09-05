# Upstream and donor synchronization

`donor/codex-switch` and `donor/codext` are read-only checkouts used to ground
the design and refresh the imported implementation. They are not build or
runtime dependencies and must not be edited as part of an MVP change. The
pinned Codext source is tracked under `runtime/codex-rs`; its source commit and
license are recorded in [`runtime/PROVENANCE.md`](../runtime/PROVENANCE.md).
The [patch ledger](../runtime/PATCH_LEDGER.md) lists each narrow integration
addition and the Codext authority it delegates to.

The controller account manager's donor mapping is recorded in
[`account-manager.md`](account-manager.md). It adapts the pinned
`humeo/codex-switch` profile store, refresh/write-back, switch, and CLI
semantics into `controller/internal/accounts` and
`controller/internal/credentials`, while `codext` remains the native
AuthManager/login service authority. The manager has no shell-out fallback to
an external Codex installation.

## Refresh procedure

1. Fetch the relevant upstream refs in a separate donor checkout.
2. Re-run the source audit for AuthManager reload, running-turn guard,
   transport invalidation, rate-limit notifications, and recovery ordering.
3. Re-check protocol payloads against the current runtime schema.
4. Update the optional runtime provenance, adapter patch ledger, and focused
   Rust tests.
5. Run `verify.ps1`, Go tests, Rust tests, and the integration module.
6. Review the diff to confirm no donor file changed and no credential value
   entered controller logs or journal records.

CodexMarathon should reapply a small patch onto a fresh upstream runtime rather
than merge unrelated Codext history. If a native symbol moves, update the
mapping and tests first; do not compensate by duplicating the mechanism in Go.

## Compatibility rules

Protocol `VERSION` is the shared compatibility integer. The first request on a
connection is `protocol/negotiate`. Every transition event carries
`transition_id`, `runtime_id`, and `auth_generation`. Unknown event fields are
forward-compatible; unknown semantics are not eligible for policy decisions.
