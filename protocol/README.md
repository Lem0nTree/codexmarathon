# Marathon protocol v1

`VERSION` is the compatibility integer for this contract. `protocol.json`
defines the JSON-RPC 2.0 envelopes. `commands.json` defines the controller to
runtime request methods and their typed params/results. `events.json` defines
the flat event params sent in a `codexmarathon/event` notification.

Streams use newline-delimited JSON. A peer must negotiate a common version via
`protocol/negotiate` before normal commands are sent. Runtime events always
include `event_type`, `occurred_at`, `runtime_id`, and `auth_generation`.
Transition events additionally include `transition_id`.

The `account/login`, `account/authSnapshot/read`, and `account/refresh` methods
are native-auth seams for the embedded Codex runtime. Login and snapshot read
return an opaque `auth_json` value only over the authenticated in-process/local
protocol connection so the controller can place it in its protected vault.
Refresh accepts the same opaque snapshot and returns only write-back token
fields. None of these payloads belongs in the controller journal, registry,
logs, or CLI output.

Codext owns the `UsageLimitExceeded` synthetic recovery turn. It emits
`recovery_parked` with a recovery/thread correlation, and the controller may
send `recovery/release` only after a committed identity-changing transition.
The release is idempotent by `recovery_id`; `already_released` is success, and
`uncertain` remains a durable retry intent. The controller never sends the
resume prompt or drains Codext's user-input queue.

The nested rate-limit shape intentionally preserves Codext app-server's
camelCase members (`rateLimits`, `limitId`, `usedPercent`, and so on), while
Marathon correlation and controller state fields use snake_case. Unknown
upstream fields are forward-compatible and must not be used by policy until
they have a typed meaning.
