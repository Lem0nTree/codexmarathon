# Native account manager

CodexMarathon owns the account/profile lifecycle in the controller. A user
does not install or invoke `codex-switch`, `codext`, or a second `codex`
executable to manage accounts.

## Reused donor behavior

The implementation is adapted from the pinned read-only checkout at
`donor/codex-switch` (source commit `ca5c7dd454780272ed429f6fb1dceebda44397ae`):

| Donor path | CodexMarathon adaptation |
| --- | --- |
| `internal/profile/profile.go` | `controller/internal/credentials` opaque snapshots plus `accounts.Manager` profile lifecycle |
| `internal/auth/refresh.go` | `accounts.AuthService.Refresh`; the installed Codex login/AuthManager performs provider refresh when its supported local interface is available |
| `internal/switcher/switcher.go` | `credentials.AtomicDeployer` and `accounts.Manager.Activate` with rollback |
| `internal/cli/auth.go` | `accounts login/add` commands through the native runtime service; no shell-out login |
| `internal/cli/list.go` and `profile_rows.go` | secret-free account list/status views backed by registry and vault metadata |
| `internal/cli/remove.go` | active-profile protection and explicit force removal |
| `internal/cli/use.go` | `accounts activate/use` commands |

The account manager also consumes the installed Codex CLI as the native
authentication authority when it exposes the supported local control
interface. Its login service is a direct boundary rather than a Go
reimplementation of OAuth or a shell command. The imported Codex-derived
runtime under `runtime/codex-rs` implements the same seam for optional
development and diagnostics.

For a stock Codex installation that is already authenticated, `accounts
import` reads the configured `auth.json`, validates only its JSON object and
optional account identity, and stores the document as an opaque snapshot. It
does not require a runtime endpoint or expose token fields.

## Storage and mutation rules

* `accounts.json` stores only stable IDs, aliases, telemetry markers, and a
  credential reference. Token fields are rejected from metadata.
* The credential vault stores one opaque validated auth snapshot per stable
  account ID with owner-only permissions.
* Login saves the snapshot before selecting it; the first account is selected
  automatically, later accounts require `--activate` or `accounts activate`.
* Import uses the same protected save and activation path as login, but takes
  the existing authenticated snapshot instead of invoking a native login.
* Activation deploys `auth.json` atomically, then updates the active marker. A
  failed or cancelled activation restores the prior file and marker.
* Refresh returns token fields only from the native service, merges non-empty
  fields while retaining unknown auth fields, and atomically redeploys the
  active snapshot.
* Removing the active profile is rejected unless `--force` is explicit. Force
  clears controller metadata but leaves the deployed `auth.json` untouched so
  deleting a local profile does not unexpectedly log Codex out.
* Native provider errors are intentionally reduced to generic operation errors
  at the manager boundary; free-form provider text is not printed because it
  could contain bearer material.

## Runtime boundary

`controller/app.RuntimeAuthService` maps the manager to the local-control
methods `account/login` and `account/refresh`. The response carries an opaque
snapshot only in memory; it must travel over the authenticated user-scoped
boundary and must never be logged or journalled. If the installed Codex does
not expose the live interface, the controlled restart path must stop the same
Codex executable before deploying the snapshot and resuming its conversation.
