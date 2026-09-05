# Companion architecture

## Outcome

CodexMarathon is a small companion to a Codex CLI that is already installed
on the machine. It coordinates account selection and safe process handoff; it
does not ship a second Codex runtime in the normal release.

```text
operator
   |
   v
codexmarathon (Go companion)
   |-- account registry and protected credential vault
   |-- quota policy, reset scheduler, journal, reconciliation
   |-- installed Codex discovery and compatibility checks
   |
   +--> installed Codex local control interface (preferred)
   |      safe boundary -> auth reload -> transport invalidation -> verify
   |
   `--> installed Codex process fallback
          graceful stop -> atomic auth deployment -> relaunch -> resume -> verify
```

The imported Rust runtime under `runtime/codex-rs/` remains in the repository
for protocol development, adapter tests, provenance, and an explicitly
optional self-contained archive. It is not a default runtime dependency and
the companion never silently substitutes it for the user's `codex` command.

## Ownership

| Concern | CodexMarathon companion | Installed Codex |
| --- | --- | --- |
| Account aliases and profile metadata | owns | observes target |
| Opaque credential snapshots | owns in protected vault | reads active auth |
| Active account choice and transition intent | owns | reports result |
| Running-turn count and safe boundary | consumes | authoritative |
| Auth reload and transport invalidation | requests through supported interface | owns |
| Conversation files and thread identity | records resume identity | owns |
| Recovery queue and exactly-once continuation | observes/correlates | owns |
| Quota telemetry and reset policy | owns policy and ranking | supplies active observations |
| Transition journal and uncertain reconciliation | owns | returns evidence |

The companion must never write `auth.json` while Codex has an active turn. A
transition first attempts the supported local control interface. If it is
unavailable or incompatible, the companion controls a graceful process stop,
performs the atomic deployment, relaunches the same installed executable, and
passes the captured conversation/thread resume identity.

## Integration boundary

The protocol schemas in [`protocol/`](../protocol/) describe the local
control messages for a compatible Codex build. A compatible implementation
must expose readiness, safe-boundary, auth reload, transport invalidation, and
identity-generation evidence. The companion verifies the target identity
after the reload before releasing a parked recovery continuation.

The fallback path has the same commit rule. A clean process exit and a disk
write are insufficient; the resumed Codex process must report the target
account and the expected generation/thread identity. If the process exits or
the acknowledgement is lost after a state-changing step, the journal keeps
the transition unresolved and reconciliation retries the same transition
identity instead of starting a second one.

No donor checkout is read at runtime. The companion's installed-Codex path is
resolved from `--codex`, configuration, or `PATH`, and its version/capability
check fails with an actionable error when the selected binary is unsuitable.
