# Marathon protocol v1

`VERSION` is the compatibility integer for this contract. `protocol.json`
defines the JSON-RPC 2.0 envelopes. `commands.json` defines the controller to
runtime request methods and their typed params/results. `events.json` defines
the flat event params sent in a `codexmarathon/event` notification.

Streams use newline-delimited JSON. A peer must negotiate a common version via
`protocol/negotiate` before normal commands are sent. Runtime events always
include `event_type`, `occurred_at`, `runtime_id`, and `auth_generation`.
Transition events additionally include `transition_id`.

The nested rate-limit shape intentionally preserves Codext app-server's
camelCase members (`rateLimits`, `limitId`, `usedPercent`, and so on), while
Marathon correlation and controller state fields use snake_case. Unknown
upstream fields are forward-compatible and must not be used by policy until
they have a typed meaning.
