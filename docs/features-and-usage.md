# CodexMarathon features and usage

This guide explains what CodexMarathon is useful for, how to install it, and
how to use it beside an existing Codex CLI.

## The problem it solves

Codex accounts are often tied to different jobs. A developer may have a work
account, a personal account, and an account for an open-source project. When
one account reaches a limit, manually changing credentials can interrupt the
conversation and make it easy to resume the wrong session.

CodexMarathon gives those accounts names and a controlled handoff. It watches
the usage information available from Codex, chooses an eligible profile, and
keeps the conversation associated with the same Codex thread.

The important design choice is that CodexMarathon is a companion. The Codex
CLI is installed separately and remains the program that talks to the model.
The companion stays small and works beside that installation.

## Features

### Multiple account profiles

Store several account profiles with stable IDs and friendly aliases such as
`work`, `personal`, or `opensource`. Profile metadata is separate from the
opaque credential snapshot used for deployment.

### Usage-aware account selection

The controller can combine active Codex rate-limit observations with stored
account observations. It ranks fresh, usable profiles and avoids selecting an
account that is known to be exhausted or cooling down.

### Safe live switching

When the installed Codex build exposes the supported local control interface,
CodexMarathon asks Codex to perform the change at a safe boundary. Codex owns
the active-turn guard, authentication reload, account-bound transport
invalidation, and identity report. The companion verifies the reported account
before it considers the change complete.

### Controlled restart and resume

When live reload is unavailable, CodexMarathon can use the process it launched:

```text
safe boundary -> graceful stop -> atomic credential deploy
             -> start the same codex executable -> resume the same thread
             -> verify the target identity
```

The process manager escalates from interrupt to termination only after a
bounded wait. It never scans for or kills an unrelated Codex process. This
ownership rule prevents an ordinary `codex` session from being interrupted by
an independently started companion.

### Continuation without duplicate prompts

The transition and resume target are journaled as one operation. CodexMarathon
does not create a second synthetic prompt when Codex already owns the recovery
turn. If a process or acknowledgement disappears, the journal provides the
information needed for reconciliation.

### Reset and recovery policy

If every known account is exhausted, the policy can wait for the earliest
trustworthy reset time and revalidate usage before selecting an account. Stale
observations are not treated as proof of available quota.

### Small, safe distribution

The archive contains the companion and its documentation. It never contains
your `auth.json`, credential snapshots, journals, or controller state.

## Install CodexMarathon

Install and authenticate Codex using its normal distribution first:

```bash
codex --version
codex login
```

Download the matching CodexMarathon archive from the
[GitHub releases page](https://github.com/Lem0nTree/codexmarathon/releases).
Linux archives use `linux-x86_64` for amd64 and `linux-aarch64` for ARM64.

Install the companion in a user-owned directory:

```bash
tar -xzf codexmarathon-<version>-linux-<arch>.tar.gz
cd codexmarathon-<version>-linux-<arch>
install -m 0755 codexmarathon "$HOME/.local/bin/codexmarathon"
```

Check that both commands are available:

```bash
codex --version
codexmarathon --help
```

CodexMarathon does not overwrite the `codex` executable. It does not copy
credentials into the installation directory.

## Initialize and inspect the companion

Create the controller-owned state directories and inspect their status:

```bash
codexmarathon init
codexmarathon status
codexmarathon status --json
codexmarathon accounts list
```

For automation, use `--json` and provide explicit paths:

```bash
codexmarathon status \
  --state-dir "$HOME/.config/CodexMarathon" \
  --auth "$HOME/.codex/auth.json" \
  --json
```

Status output contains account IDs, health, policy, and lifecycle information.
It does not print access or refresh tokens.

## Start Codex through the companion

Starting a conversation through CodexMarathon gives the companion ownership of
that process. Ownership is required for the restart/resume fallback:

```bash
codexmarathon run --codex codex
```

Pass a working directory and normal Codex options explicitly:

```bash
codexmarathon run \
  --codex "$HOME/.local/bin/codex" \
  --codex-home "$HOME/.codex" \
  --codex-dir "$HOME/projects/my-app" \
  --codex-arg=--no-alt-screen \
  --codex-arg=--model \
  --codex-arg=gpt-5
```

Arguments supplied with `--codex-arg` are passed as separate argument values;
they are not interpreted by a shell. Positional values after the options are
also forwarded to Codex when the command-line parser accepts them.

If Codex is already running with a compatible local control channel, attach to it:

```bash
codexmarathon run --attach-only
```

Without `--attach-only`, the command starts the installed Codex executable if
no compatible local control endpoint is available.

## Manage profiles

List profiles and inspect one profile:

```bash
codexmarathon accounts list
codexmarathon accounts status work
```

Rename a local alias or select a profile manually:

```bash
codexmarathon accounts rename --name work account-id
codexmarathon accounts activate account-id
codexmarathon accounts use account-id
```

Remove a profile when it is no longer needed:

```bash
codexmarathon accounts remove account-id
```

The account ID in these examples is the ID printed by `accounts list`; the
word `work` is an example alias, not a special built-in account.

## Add or refresh credentials

The account login and refresh commands use the Codex authentication service
through a compatible Marathon runtime endpoint:

```bash
codexmarathon accounts login \
  --runtime unix:///path/to/marathon-runtime.sock \
  --name work \
  --activate

codexmarathon accounts refresh \
  --runtime unix:///path/to/marathon-runtime.sock account-id
```

The endpoint is a local IPC endpoint, not a public network service. Do not
place token values in `--runtime` or any other command argument. If your
installed Codex release does not expose the compatible login endpoint, use its
normal `codex login` flow and follow the repository's current onboarding
instructions before adding the resulting profile.

## Switch accounts

For a live installed Codex session, request a switch by account ID:

```bash
codexmarathon switch account-id
```

You can specify the installed executable, Codex home, or control endpoint:

```bash
codexmarathon switch \
  --codex "$HOME/.local/bin/codex" \
  --codex-home "$HOME/.codex" \
  --app-server-endpoint unix:///tmp/codex-control.sock \
  account-id
```

The preferred path asks Codex to reload at a safe boundary. If the endpoint
does not support auth reload, the switch can use restart/resume only when the
Codex process was started by `codexmarathon run` in the same supervisor.

## A normal workday

Here is a complete example for a developer with work and personal accounts:

```bash
# One-time setup
codexmarathon init
codexmarathon accounts list

# Start a project using the installed Codex CLI
codexmarathon run --codex codex --codex-dir "$HOME/projects/api"

# Check the active account from another terminal
codexmarathon status --json

# Make a deliberate change if needed
codexmarathon switch work-account
```

For automatic selection, leave the companion session running. It consumes the
installed Codex event stream, feeds fresh observations into the policy loop,
and invokes the live or restart transition at the appropriate boundary.

## Troubleshooting

### `installed Codex CLI was not found`

Check the executable and pass it explicitly:

```bash
command -v codex
codexmarathon run --codex /full/path/to/codex
```

You can also set `CODEX_BIN` for scripts.

### The companion cannot attach

Confirm the Codex home and endpoint:

```bash
echo "$CODEX_HOME"
codexmarathon run --codex-home "$HOME/.codex" --attach-only
```

If the installed version has no compatible local control interface, launch it
through `codexmarathon run` so the controlled restart/resume path is available.

### A restart is refused

CodexMarathon refuses to signal a process it did not start. Stop the existing
Codex session normally, then start it through the companion. This is a safety
boundary that protects unrelated sessions.

### A command asks for `--runtime`

The login and refresh paths require a compatible Marathon authentication
endpoint. The normal installed-Codex process path uses `run` and `switch`.

## Developer utilities

Build the companion and run the deterministic checks:

```bash
bash scripts/build-release.sh
(cd controller && go test ./...)
(cd controller && go vet ./...)
(cd integration && go test ./...)
python3 scripts/test_package_release.py
python3 scripts/verify_protocol.py --root .
python3 scripts/verify_provenance.py --root .
```

Verify a release archive from a clean environment:

```bash
python3 scripts/verify_package.py --artifact dist/release/<artifact>.tar.gz
python3 scripts/clean_machine_check.py --artifact dist/release/<artifact>.tar.gz
```

## Security model

CodexMarathon treats credential snapshots as opaque data. It uses restrictive
permissions and atomic replacement for controller-owned credential deployment.
It avoids putting secrets in command output, journals, generated manifests,
or archives.

The companion cannot make a third-party or unrelated process reload a secret by
force. Live reload must be accepted by the installed Codex control interface;
otherwise only a process owned by the companion can be restarted and resumed.

## Further reading

- [Main project README](../README.md)
- [Companion process design](companion-cli.md)
- [Architecture and ownership](architecture.md)
- [Build and verification](build-test.md)
- [Release checklist](release.md)
- [Project completion plan](../PLAN.md)
