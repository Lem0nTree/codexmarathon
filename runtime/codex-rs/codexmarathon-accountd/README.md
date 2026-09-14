# codexmarathon-accountd

`codexmarathon-accountd` is a per-user, metadata-only daemon. It reads typed
quota snapshots from `codexmarathon-runtime` and serves a versioned
newline-delimited JSON protocol on an owner-only Unix stream socket.

The daemon prefers systemd socket activation (`LISTEN_PID`, `LISTEN_FDS=1`,
fd 3). For foreground development, pass an explicit socket path:

```text
codexmarathon-accountd --socket /run/user/1000/codexmarathon-accountd/accountd.sock
```

The database defaults to `$CODEX_HOME/marathon/quota.sqlite` (or
`$HOME/.codex/marathon/quota.sqlite`) and can be overridden with `--quota-db`.
The only public methods are read-only:

```json
{"version":1,"id":1,"method":"health","params":{}}
{"version":1,"id":2,"method":"status","params":{}}
{"version":1,"id":3,"method":"accounts","params":{}}
{"version":1,"id":4,"method":"events_since","params":{"after":0,"limit":100}}
```

Each request receives one JSON response line. Requests are bounded to 64 KiB
and each read, handler, and write has a finite timeout. The SQLite event and
job tables are private daemon metadata in the same quota database. Job writes
are library seams for a future constrained executor; no job mutation is
available over the socket. Interrupted running jobs are reconciled at startup:
jobs with a valid idempotency key enter `retry_wait`, while unsafe jobs fail
with the bounded `unsafe_retry` diagnostic.
