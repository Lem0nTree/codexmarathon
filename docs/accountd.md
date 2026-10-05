# `codexmarathon-accountd`

Status: the metadata-only SQLite quota store, app-server ingestion, accountd
binary, durable events/jobs, and local API are implemented. The banked-reset
expiry policy adds durable scheduling and a separate native executor. The
older exhaustion-based autospin policy remains mock-only.

## Decision

`codexmarathon-accountd` is a long-lived, user-scoped metadata daemon and
scheduler. It is separate from the native reset executor. Accountd owns account identifiers and aliases, quota observations,
job intent, durable job state, scheduling, and a replayable event stream. It
does not own credentials, provider authentication, a Codex turn, or arbitrary
process execution.

The deployment consists of the paired
`codexmarathon-accountd.service` and `codexmarathon-accountd.socket` units:

* The service is enabled in `default.target`, remains running for scheduling,
  and uses `Type=exec`. The release installer verifies readiness through the
  daemon's versioned health/account API after systemd reports it active.
* The socket unit owns the single local AF_UNIX stream listener at
  `%t/codexmarathon-accountd/accountd.sock`, with mode `0600` and a `0700`
  parent directory. There is no TCP listener or externally reachable API.
* Socket activation supplies the listener to the already target-enabled
  scheduler (`LISTEN_FDS`/`LISTEN_PID`); it is not used to make scheduler
  lifetime demand-driven. The daemon must not bind or unlink that path itself.
  Enable both units together. If a future implementation cannot consume
  systemd activation file descriptors, remove the socket unit and change the
  service and protocol in one reviewed change; never run two competing
  listeners.

The service reserves a private systemd user-state directory through
`StateDirectory=codexmarathon-accountd`. Its current quota, event, and job
tables share `$CODEX_HOME/marathon/quota.sqlite` (normally
`$HOME/.codex/marathon/quota.sqlite`) with app-server quota ingestion. SQLite
files and their WAL or journal sidecars are owner-only. `UMask=0077`, private
directory checks, and the socket mode are defense in depth; accountd still
rejects credential-shaped fields and avoids logging secrets. The unit runs as
the invoking user, not as a system service or root.

## Ownership boundary

```text
local clients
     |  AF_UNIX, owner-only permissions
     v
accountd: metadata + SQLite + scheduler + events
     |  vetted metadata job / acknowledgement
     v
constrained native executor: Codex authority and side effects
```

Accountd may queue and schedule an allow-listed job description, but it must
not receive tokens, cookies, authorization headers, credential snapshots, or
raw shell commands. The native executor is a separate trust boundary:
it validates the job, owns any native Codex authority it needs, applies its
own sandbox and idempotency rules, and returns status/acknowledgement data.
The accountd units start no executor and make no provider calls. Releases also
install `codexmarathon-reset-executor.service`, a separate native authority with
credential access and HTTPS capability. Its expiry policy is disabled by
default. Accountd keeps its existing credential and network restrictions.

## Banked reset expiry

`auto_reset_expiry_status` and `auto_reset_expiry_set` expose the persistent
expiry policy. Setting requests include the expected Codex home, which the
daemon checks before mutation. Native app-server methods expose the same
policy to integrations. Existing version 1 read methods remain compatible.

The narrow executor methods `expiry_executor_heartbeat`, `expiry_observe`,
`expiry_claim`, and `expiry_complete` carry only typed metadata. Credits are
bound to a managed account and an opaque provider credit ID. Expiration comes
from the provider inventory, never a quota-window reset timestamp. Each
logical redemption retains one idempotency key, including after a crash.
Claims have leases and completion tokens so an old executor cannot acknowledge
a newer claim. The scheduler attempts available credits at expiration minus
900 seconds; overdue unexpired credits are eligible immediately.

Retries use 5, 10, 20, then 30-second delays until the credit expires or the
policy is disabled. Missing inventory details do not erase prior observations
or establish redemption. A confirmed provider redemption is persisted before
subsequent quota refresh so refresh failure cannot cause another credit to be
spent. Success and retry diagnostics are available through the existing event
stream and expiry status. All development tests use fake metadata/providers.

## Readiness and events

Systemd process readiness is separate from the application event stream. The
release installer verifies the daemon API after startup; clients use that same
versioned local API as their readiness boundary. Accountd may emit
non-sensitive status notifications on compatible service managers. No
credential or provider response may appear in a notification or journal line.

Durable events are committed in the same SQLite transaction as the state change
they describe. Each event has a monotonic event ID, event kind, timestamp, and
optional job/account identifier. Consumers resume from an event ID and treat
redelivery as harmless. The initial event vocabulary is:

* `account.observed` and `quota.observed` for metadata supplied by an
  explicitly authorized native boundary;
* `job.queued`, `job.started`, `job.retry_wait`, `job.succeeded`,
  `job.failed`, and `job.cancelled`;
* `executor.ready`, `executor.unavailable`, and `autospin.blocked`.

Events carry identifiers, state, generation, timing, and bounded diagnostic
codes only. They never carry credential material, arbitrary command text, or
unredacted provider payloads.

## Job state machine

The durable states are:

```text
queued -> running -> succeeded
   |         |          (terminal)
   |         +-------> failed (terminal)
   |         +-------> cancelled (terminal)
   +-------> cancelled (terminal)
   |
   +-------> retry_wait -> queued
```

`running` is reconciled on startup: an interrupted job is not guessed to have
succeeded. It becomes `retry_wait` only when the executor contract and
idempotency key make retry safe; otherwise it becomes `failed` with a bounded
diagnostic. A terminal result is durable before its success/failure event is
published. Cancellation is explicit and must not interrupt an unsafe native
side effect.

## Autospin safety gate

Autospin is experimental and off by default. Nothing in these units enables
it, and a missing or malformed setting must resolve to disabled. Accountd may
record an `autospin.blocked` event, but it must not call a provider or infer
that a quota reset is available merely because time elapsed.

An explicit operator opt-in is necessary but not sufficient. Before any
production autospin action, a versioned provider contract must define the
supported capability, authentication owner (the native executor, never
accountd), endpoint and allowed operation, idempotency behavior, rate/terms
constraints, dry-run behavior, timeout, rollback or recovery path, and the
post-action quota revalidation evidence. The constrained executor must enforce
that contract and reject unknown providers, stale versions, missing evidence,
or unsupported account modes. The exhaustion-based autospin implementation remains mock-only. The separate
banked-reset expiry policy uses the existing native reset API after explicit
opt-in; its tests use disposable metadata and simulated provider responses.

## Original autospin delivery phases

1. **Foundation:** land the units and this design; implement private SQLite
   metadata, migrations, event replay, readiness, and crash reconciliation.
   No credentials, native side effects, provider calls, or autospin.
2. **Executor boundary:** add a separately reviewed constrained native
   executor and a typed local job/ack protocol with allow-lists, deadlines,
   idempotency, and audit events.
3. **Experimental dry run:** add a versioned provider-contract adapter and
   fake-executor tests. Keep autospin disabled and expose only dry-run/audit
   outcomes.
4. **Explicit production opt-in:** require security, provider-policy, failure
   recovery, quota-revalidation, and operational review before enabling a real
   provider capability. Rollback must be possible without changing accountd's
   metadata ownership boundary.

## Installation and operations

Build and install the account daemon, reset executor, and CLI, then install
the user units. Run these commands from the repository root:

```bash
cd runtime/codex-rs
cargo build --release --bin codexmarathon-accountd --bin codexmarathon-reset-executor --bin codex
install -Dm0755 target/release/codexmarathon-accountd \
  "$HOME/.local/bin/codexmarathon-accountd"
install -Dm0755 target/release/codex "$HOME/.local/bin/codex"
install -Dm0755 target/release/codexmarathon-reset-executor \
  "$HOME/.local/bin/codexmarathon-reset-executor"
cd ../..
install -Dm0644 infra/systemd/user/codexmarathon-accountd.service \
  "$HOME/.config/systemd/user/codexmarathon-accountd.service"
install -Dm0644 infra/systemd/user/codexmarathon-accountd.socket \
  "$HOME/.config/systemd/user/codexmarathon-accountd.socket"
install -Dm0644 infra/systemd/user/codexmarathon-reset-executor.service \
  "$HOME/.config/systemd/user/codexmarathon-reset-executor.service"
systemctl --user daemon-reload
systemctl --user enable --now codexmarathon-accountd.socket codexmarathon-accountd.service codexmarathon-reset-executor.service
systemctl --user status codexmarathon-accountd.service codexmarathon-accountd.socket
```

For a release archive, the one-command installer keeps the default layout at
`$HOME/.codex/marathon`. To place Marathon state elsewhere, set
`CODEXMARATHON_CODEX_HOME` before running the installer. If that variable is
unset, an existing `CODEX_HOME` is used; if both are unset, `$HOME/.codex` is
used. `CODEXMARATHON_CODEX_HOME` therefore has precedence over `CODEX_HOME`,
and an explicitly empty value is rejected. The selected path must be an
absolute, printable path that is not `/`, contains no `%` (reserved by
systemd), and has no `.` or `..` components. Spaces, quotes, and backslashes
are escaped in the generated systemd drop-in, so paths such as
`/srv/Codex Marathon` are supported.

The installer creates `<CODEX_HOME>/marathon` with owner-only permissions and
generates
`~/.config/systemd/user/codexmarathon-accountd.service.d/10-codex-home.conf`.
That drop-in sets the daemon's `CODEX_HOME`, replaces the base unit's
`ReadWritePaths=` and `InaccessiblePaths=` allowlists, and leaves the socket at
`$XDG_RUNTIME_DIR/codexmarathon-accountd/accountd.sock`; the socket never
follows `CODEX_HOME`. To return to the default layout on an upgrade, unset
both override variables and rerun the installer. It removes the generated
drop-in and restores the paths from the packaged unit. The installer does not
modify shell startup files, so set `CODEX_HOME` in the environment when
running the CLI against a custom home.

Query it without touching credentials:

```bash
codex marathon accounts --daemon --format table
codex marathon accounts --daemon --format json
```

Install the Armbian login summary after the daemon is healthy:

```bash
sudo install -Dm0755 scripts/armbian-motd-codexmarathon.sh \
  /etc/update-motd.d/42-codexmarathon
run-parts /etc/update-motd.d
```

The release installer attempts to enable user lingering automatically (first
as the user, then through passwordless sudo) so the scheduler continues after
the installing SSH session exits. If host policy permits neither method, it
fails before copying files. Confirm that policy with:

```bash
loginctl user-status "$USER"
```

Disable it later with `loginctl disable-linger "$USER"` if persistence is no
longer wanted. Linger does not grant accountd additional privileges and does
not change socket permissions.

Useful read-only checks are:

```bash
systemctl --user is-active codexmarathon-accountd.service
systemctl --user is-active codexmarathon-accountd.socket
journalctl --user -u codexmarathon-accountd.service --no-pager
```

If readiness fails, inspect the journal for SQLite recovery or listener errors
and verify that only the systemd-managed daemon is running. Do not solve a
stale socket by making it world-writable or by starting a second daemon. A
manual stop should stop both units so the next start can recreate the
owner-only endpoint.
