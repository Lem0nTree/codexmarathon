# Telemetry and quota decisions

The native Codex app-server supplies account rate-limit observations to
Marathon. The runtime keeps complete and sparse window updates separate,
attributes each observation to the account that Codex reports, and preserves
unknown provider fields without using them for policy decisions.

```text
native rate-limit observation
    -> account snapshot and sparse merge
    -> freshness and quota policy
    -> safe-boundary account switch or reset wait
```

Freshness is tracked per quota window. An elapsed reset timestamp makes the
observation stale; it does not prove that quota is available. Missing,
ambiguous, or failed observations cannot authorize a switch.

When every managed account reports zero weekly quota, the policy can consider
an account that advertises a provider-supported reset capability. Automatic
reset is disabled by default and is exercised with mock capabilities and
executors in development and tests. A real provider reset is never called by
this repository.

After any reset or refreshed observation, Marathon revalidates the complete
quota state before selecting an account. It does not treat elapsed time alone
as proof that a limit was restored.
