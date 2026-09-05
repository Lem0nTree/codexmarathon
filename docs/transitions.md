# Account transition and reconciliation

CodexMarathon records one transition ID and one expected target generation for
each account change. The installed Codex process remains authoritative for
active turns, authentication reload, model transport invalidation, and runtime
identity.

```text
read installed-Codex identity (A, N)
        |
prepare transition to B (tx, N+1)
        |
supported control: wait safe boundary, reload, invalidate
or fallback: stop Codex, deploy auth, relaunch and resume
        |
read runtime/process identity and active-auth identity
        |
commit only when both agree on B, N+1, and the same conversation
```

The companion captures refreshed fields from Account A before deployment,
atomically writes Account B's opaque snapshot, and verifies the target after
reload or after the resumed process starts. It never treats an `auth.json`
mtime, a successful file rename, or an uncorrelated process exit as proof of a
committed switch.

## Outcomes

- `committed`: installed Codex and the deployed credential agree on B at the
  expected generation, with the intended conversation identity.
- `rejected`: the installed process rejected the correlated transition before
  adoption; the previous account remains active.
- `uncertain`: a process, IPC, journal, or verification failure leaves the
  effect unknown. The journal blocks a new transition until the same ID is
  reconciled.

For an uncertain transition, reconnect to the same installed Codex process or
resume the recorded conversation, read both authorities, and resolve only
when the account/generation/thread evidence agrees. If the process was lost,
the controlled restart path uses the recorded executable and resume identity;
it does not launch a different Codex installation or submit a duplicate
recovery.

## Safety requirements

The local control transport must be user-scoped and authenticated by the
operating system. The fallback must confirm a clean process exit before
writing the active credential. Both paths must preserve Codex's own recovery
queue and release it once, after identity verification.
