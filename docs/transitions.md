# Account transitions

The native Marathon service records one transition ID and expected target
generation for each account change. Codex remains authoritative for the
running turn, authentication reload, model transport invalidation, and
conversation recovery.

```text
read native identity (A, N)
        |
prepare target B (transition, N + 1)
        |
wait for safe boundary -> atomically persist B -> reload native auth
        |
invalidate account-bound transports -> read identity
        |
commit only when B, N + 1, and the same thread agree
```

The source identity is rechecked immediately before deployment. Credential
material remains opaque to the service boundary and never enters output,
telemetry, or transition records.

## Outcomes

- `committed`: Codex and durable Marathon state agree on the target account and
  generation.
- `rejected`: Codex rejected the request before adopting the target.
- `uncertain`: a persistence, reload, or identity acknowledgement was lost;
  the same transition ID remains available for reconciliation.

An uncertain result is never inferred from a file timestamp or a successful
rename. The recovery continuation remains parked until Codex reports the
verified target identity. This preserves exactly-once continuation when the
process or an acknowledgement is interrupted.
