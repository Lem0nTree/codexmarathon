# Conversation recovery

Codex owns the conversation, thread state, parked usage-limit continuation,
and exactly-once resume. Native Marathon selects an eligible account and
coordinates the identity transition that must complete before Codex releases
its existing recovery work.

When Codex reports `UsageLimitExceeded`, the native flow is:

```text
Codex parks its existing recovery turn
        |
        v
Marathon selects a fresh eligible managed account
        |
        v
wait for safe boundary -> persist target -> native auth reload
        |
        v
invalidate account transports -> verify target identity
        |
        v
Codex releases its parked recovery turn exactly once
```

Marathon never edits the active credential during a running turn, creates a
second turn counter, copies the recovery prompt, or submits a duplicate
continuation. If no account has fresh usable quota, the recovery remains
parked while the policy waits for quota state to change and revalidates it.

The transition journal records the same transition and recovery IDs across
failures. A missing acknowledgement leaves the result uncertain until the
native service can read the account and generation evidence; it is not guessed
from a file timestamp or process exit.

## Reset behavior

Automatic reset is opt-in. It becomes eligible only after every managed
account reports zero weekly quota and at least one account exposes a supported
provider reset capability. The reset executor is disabled in the current
product and all development/tests use a fake executor. No real provider reset
is sent by this repository.
