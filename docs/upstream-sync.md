# Upstream and donor synchronization

`donor/codex-switch` and `donor/codext` are read-only checkouts used to ground
the design. They are not dependencies and must not be edited as part of an
MVP change. The standalone Rust adapter's [patch ledger](../runtime/PATCH_LEDGER.md)
lists each narrow integration addition and the Codext authority it delegates
to.

## Refresh procedure

1. Fetch the relevant upstream refs in a separate donor checkout.
2. Re-run the source audit for AuthManager reload, running-turn guard,
   transport invalidation, rate-limit notifications, and recovery ordering.
3. Re-check protocol payloads against the current runtime schema.
4. Update the adapter patch ledger and focused Rust tests.
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

