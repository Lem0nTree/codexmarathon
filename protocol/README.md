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
are native-auth seams for a compatible installed Codex runtime. Login and
snapshot read return an opaque `auth_json` value only over the authenticated
user-scoped local control connection so the companion can place it in its
protected vault. Refresh accepts the same opaque snapshot and returns only
write-back token fields. None of these payloads belongs in the controller
journal, registry, logs, or CLI output. The imported Rust runtime implements
the same seam for optional protocol development; it is not required by the
default companion package.

## TUI to controller control socket

When `codexmarathon run` owns the Codex session it also exposes a separate
user-scoped local socket for TUI commands. The default Unix endpoint is
`unix://<state-dir>/controller.sock`; a launched Codex process receives the
same value in `CODEXMARATHON_CONTROL`. This endpoint is distinct from the
runtime socket because it can initiate a credential transition.

The client first sends the same `protocol/negotiate` request with
`supported_versions: [1]`. The controller then accepts these secret-free
requests:

* `marathon/status` with `{}` returns the registered account aliases, active
  marker, runtime identity/turn count when a Marathon runtime is attached,
  and any controller transition metadata.
* `marathon/switch` with
  `{"target":"<account-id-or-alias>","timeout_ms":120000}` resolves the
  alias in the controller and runs the complete controller-owned transition.
  The request path is `prepare -> wait for the runtime's safe boundary ->
  atomically deploy the selected snapshot -> commit/reload -> verify identity`;
  the TUI never edits `auth.json`.

The switch result contains only account ID/alias, outcome, mode, generation,
transition ID, and a redacted reason. A `committed` result is safe to display
as the new active identity. An `uncertain` result carries the transition ID
for controller reconciliation and must not be treated as a successful switch.

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
