# Installed Codex companion mode

CodexMarathon can run as a companion to a user's installed `codex` command.
In this mode it does not start or ship `codex-app-server`; it owns only the
Codex process it launched and leaves the user's normal Codex home, sessions,
configuration, and CLI version in place.

When an installed Codex version exposes the Marathon reload protocol, the
controller can switch credentials at the runtime's safe boundary and keep the
current process alive. Older or unmodified versions use the controlled
restart fallback:

```text
running codex process
        |
        | SIGINT and wait for process exit
        | SIGTERM/SIGKILL only after the deadline
        v
atomic target auth.json deployment
        |
        v
codex resume <thread-id>
```

The target may use `codex resume --last` when the caller does not have a
specific thread ID. The resume target and optional CLI arguments are passed as
separate argv values; shell interpolation is never used. The process manager
does not inspect or log credentials.

The deployment callback runs only after the previous process has exited. If
deployment fails, the process remains stopped so its in-memory credentials
cannot race the failed transition. If the process does not honor the graceful
signal, the manager escalates in bounded phases before returning. A companion
supervisor never scans for or signals an unrelated Codex process; installations
that were already running before the companion started must be relaunched
through the companion to establish ownership.
