# Telemetry and reset decisions

Installed Codex rate-limit observations are the active-account authority when
the supported local control interface is available. The optional adapter
forwards a complete `rate_limits_snapshot` and sparse `rate_limits_updated`
event; the controller normalizes the upstream camelCase window fields into a
multi-bucket domain model.

```text
installed Codex event
    -> NormalizeSnapshot
    -> StateStore (sparse merge authority)
    -> SnapshotCache (deep-copy TTL cache)
    -> policy.Engine
    -> automation.Loop
    -> reset.Scheduler when the pool is exhausted
```

`rateLimits` is the aggregate view. `rateLimitsByLimitId` is the optional map
of metered buckets. The controller never assumes one fixed bucket per account.
Unknown upstream fields remain forward-compatible but have no policy meaning
until a typed model is added.

## Sparse updates

An update without `limitId` is applied only when one target bucket can be
identified. If multiple buckets are present, the update is left untouched and
the controller reports `telemetry refetch required`. A runtime connection's
account identity is the attribution source; the sparse payload itself is not
treated as an account selector.

Nullable metadata does not clear an existing value. An explicit `usedPercent: 0`
is retained as a real server observation, while an omitted usage field does
not create an invented zero-usage window.

## Freshness

Freshness is tracked independently per window. `resetsAt <= now` marks only
that window stale. It never proves that usage is zero or that capacity is
available. Provider usability is separate from window freshness, so missing
or failed telemetry cannot become an eligible account.

## Pool exhaustion

When no configured account has a complete fresh non-exhausted snapshot, policy
returns `wait_for_reset` when a future server reset is trustworthy, otherwise
`no_telemetry`. The reset scheduler stores the earliest reset as a wake-up
hint. At that boundary the controller must refresh telemetry and reevaluate;
it must not transition merely because the timestamp elapsed.

The preferred provider is event-driven from the installed Codex process.
Inactive-profile providers are pluggable through `telemetry.UsageProvider`;
`internal/quota.Provider` adapts
the donor `codex-switch/internal/quota` calibration request and retry/header
rules to that interface. `app.NewAutomationLoop` installs that provider for
stored accounts that do not already have an active runtime snapshot provider,
so inactive profiles are checked without starting a second Codex process. The
active installed-Codex provider remains the event/state-store authority and
replaces the fallback when its authoritative snapshot event arrives.

## Automatic policy

`internal/automation.Loop` is the feature boundary that turns observations
into an action. It refreshes every configured account once per event, maps
`threshold_reached`, `usage_limit_exceeded`, and reset wakes to explicit
policy triggers, and calls the transition layer only for a deterministic
replacement. A provider error stays attached to that account and cannot be
interpreted as available quota.

The policy engine carries a one-minute donor-compatible cooldown, remembers
event IDs, and suppresses an in-flight target. When all accounts are
unavailable it returns the earliest future server reset plus the account that
must be revalidated; when no reset is trustworthy it returns a bounded data
retry hint. A wake-up never marks quota restored by elapsed time alone.
