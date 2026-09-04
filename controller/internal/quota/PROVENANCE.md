# Quota implementation provenance

`controller/internal/quota` is a product-owned, standard-library-only
adaptation of the public `humeo/codex-switch` quota checker at donor commit
`ca5c7dd454780272ed429f6fb1dceebda44397ae`.

The low-level request/header behavior was adapted from:

- `donor/codex-switch/internal/quota/quota.go`
- `donor/codex-switch/internal/quota/quota_test.go`
- `donor/codex-switch/internal/cli/list.go` (cache freshness and reset metadata)

The account-selection policy, threshold, cooldown, event de-duplication, and
reset-wait behavior were independently adapted into the CodexMarathon
controller from the donor watcher package:

- `donor/codex-switch/internal/watcher/watcher.go`
- `donor/codex-switch/internal/watcher/events.go`
- `donor/codex-switch/internal/watcher/state.go`

CodexMarathon does not import donor modules at build time. The adapter keeps
credentials opaque, never writes tokens to telemetry/cache state, and redacts
credential-bearing values from provider errors. Changes made here should be
reviewed against the pinned donor commit when upstream behavior changes.
