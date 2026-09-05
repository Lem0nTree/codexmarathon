# Conversation recovery with an installed Codex CLI

Codex remains the authority for the conversation, thread state, parked
recovery turn, and exactly-once continuation. CodexMarathon only chooses a
target account and coordinates the identity transition that must happen before
Codex releases its existing recovery work.

When Codex reports `UsageLimitExceeded`, the sequence is:

```text
Codex parks its existing recovery turn
        |
        v
companion selects a fresh eligible account
        |
        +--> supported local control interface
        |      safe boundary -> reload -> invalidate -> verify identity
        |
        `--> controlled process handoff
               graceful stop -> atomic auth deploy -> relaunch codex
               -> resume same thread -> verify identity
        |
        v
Codex releases its parked recovery turn exactly once
```

The companion never edits `auth.json` during an active turn, creates another
turn counter, copies the recovery prompt, or submits a second continuation.
If no account has usable fresh telemetry, the recovery stays parked while the
reset scheduler waits and then revalidates quota after the reset timestamp.

## Supported local control path

For a compatible installed Codex version, the companion opens its protected
user-scoped local control endpoint. It correlates a transition ID and expected
auth generation, waits for the runtime-owned safe boundary, requests the
native authentication reload and transport invalidation, and reads the
resulting runtime identity. Recovery is released only after the target account,
generation, and deployed credential identity agree.

Lost replies remain unresolved in the journal. Reconnect/reconciliation retries
the same transition ID and never starts a new recovery turn.

## Controlled restart path

When the installed Codex version has no compatible local control interface,
the companion uses a controlled process boundary:

1. Ask the Codex process to stop cleanly at its normal boundary and persist
   the conversation/thread resume identity.
2. Confirm the process has exited before changing the active credential file.
3. Atomically deploy the selected profile snapshot and preserve any refreshed
   fields written by the old process.
4. Relaunch the same installed `codex` executable with the captured resume
   identity and the original user arguments.
5. Verify that the resumed process reports the target account before allowing
   Codex's parked recovery turn to run.

If a restart fails after deployment, journal the transition as uncertain and
reconcile the same process/thread identity after the next launch. A timestamp
or file existence check never proves that the new process adopted the target
identity.
