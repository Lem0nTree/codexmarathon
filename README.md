# CodexMarathon

CodexMarathon is a small companion for the Codex CLI you already use.

It helps you keep more than one Codex account available, watch account usage,
and move a conversation to another account when the current account reaches a
limit. Your installed `codex` command remains the Codex client. CodexMarathon
does not replace it, and the normal release does not contain a second
`codex-app-server`.

## Why use it?

Many developers use Codex for work, personal projects, open-source work, or a
team account. Each account has its own authentication and usage allowance.
Without a coordinator, changing accounts usually means stopping work, finding
the right credential file, replacing `auth.json`, and remembering how to get
back to the same conversation.

CodexMarathon keeps those details in one place. It selects an account, waits
for a safe point in the current Codex turn, and hands the conversation back to
the installed Codex CLI. The result is less manual file handling and fewer
lost or duplicated prompts.

## A concrete example

Suppose you use a personal account for side projects and a work account for
your company. You save both profiles in CodexMarathon and start Codex through
the companion:

```bash
codexmarathon accounts list
# personal   Personal account
# work       Work account

codexmarathon run --codex codex --codex-dir ~/projects/website
```

While you work, CodexMarathon watches the usage information reported by the
installed Codex process. When `personal` reaches its limit, the companion can
choose `work` according to the configured policy. If the installed Codex
version supports the local control interface, the account is reloaded at a
safe boundary and the same process continues. If it does not, CodexMarathon
stops the process cleanly, deploys the selected profile, launches the same
installed `codex` executable, and resumes the same conversation.

You can also request a manual change:

```bash
codexmarathon switch work
```

The companion never writes credentials behind an active turn. A restart is
used only for a Codex process that CodexMarathon started itself; it never
signals an unrelated Codex process owned by the user.

## How the companion fits with Codex

```text
you
 |
 +--> codexmarathon  (account profiles, quota policy, safe handoff)
          |
          +--> installed codex app-server (preferred live reload)
          |
          `--> installed codex process (controlled restart and resume)
```

Codex remains responsible for its own conversation files, active-turn state,
authentication manager, model transports, and resume behavior. CodexMarathon
adds account selection and coordination around those existing responsibilities.

The handoff has two modes:

1. **Live reload.** The companion talks to the installed Codex local control
   interface, waits for its safe boundary, asks it to reload authentication,
   and verifies the new identity before continuing.
2. **Controlled restart.** When live reload is unavailable, the companion
   gracefully stops the Codex process it owns, atomically deploys the target
   credential, starts the same executable, and resumes the selected thread.

Both modes keep the account change tied to one transition and one conversation.
The companion does not create a second resume prompt.

## Benefits for users

- Keep work and personal Codex accounts available without copying credential
  files by hand.
- Continue a long task after an account reaches a usage limit.
- Use the exact Codex version, configuration, plugins, and session files
  already on your machine.
- Switch manually when you choose, or let the quota policy select a usable
  account.
- See account and transition status without printing tokens.
- Use a small release: the normal Linux ARM64 package is about 5 MB and does
  not duplicate the Codex runtime.
- Keep the option of an explicit embedded-runtime package for protocol work or
  offline diagnostics.

## Install

Install Codex first using its normal distribution. Confirm that it works:

```bash
codex --version
```

Download the CodexMarathon archive for your Linux machine from the
[GitHub releases page](https://github.com/Lem0nTree/codexmarathon/releases).
The current release naming is `linux-x86_64` for amd64 and `linux-aarch64`
for ARM64.

Extract and install the companion:

```bash
tar -xzf codexmarathon-<version>-linux-<arch>.tar.gz
cd codexmarathon-<version>-linux-<arch>
install -m 0755 codexmarathon "$HOME/.local/bin/codexmarathon"
codexmarathon init
codexmarathon status --json
```

If `$HOME/.local/bin` is not on your `PATH`, add it using your shell's normal
configuration. The archive does not replace `codex`, modify the Codex
installation, or include your account data.

## Quick start

Discover the installed Codex command and inspect the local state:

```bash
codexmarathon status --json
codexmarathon accounts list
```

If `codex` is not on `PATH`, pass its location explicitly:

```bash
codexmarathon run --codex "$HOME/.local/bin/codex"
```

You can select the Codex home and working directory explicitly as well:

```bash
codexmarathon run \
  --codex "$HOME/.local/bin/codex" \
  --codex-home "$HOME/.codex" \
  --codex-dir "$HOME/projects/my-app"
```

Normal Codex arguments can be forwarded with `--codex-arg`:

```bash
codexmarathon run --codex-arg=--no-alt-screen --codex-arg=--model --codex-arg=gpt-5
```

Use `--attach-only` when you want the command to connect to an already
running compatible Codex app-server and fail instead of launching another
Codex process:

```bash
codexmarathon run --attach-only
```

## Account commands

The account manager stores non-secret profile metadata separately from opaque
credential snapshots. Useful commands are:

```text
codexmarathon accounts list
codexmarathon accounts status [account-id]
codexmarathon accounts rename --name <alias> <account-id>
codexmarathon accounts activate <account-id>
codexmarathon accounts use <account-id>
codexmarathon accounts remove <account-id>
```

Native login and refresh use the integrated Codex authentication service. The
current command surface also accepts an explicit `--runtime` endpoint for
installations that expose the Marathon authentication protocol:

```bash
codexmarathon accounts login --runtime unix:///path/to/runtime.sock
codexmarathon accounts refresh --runtime unix:///path/to/runtime.sock <account-id>
```

Do not paste access tokens into command arguments. The companion treats
credential content as opaque data and keeps it out of status output, journals,
and release archives.

## Manual switching and resume

With a compatible installed app-server, switch a live Codex session by account
ID:

```bash
codexmarathon switch <account-id>
```

The command uses the installed Codex local control endpoint by default. You can
provide an explicit endpoint or executable path when the installation uses a
non-default layout:

```bash
codexmarathon switch \
  --codex "$HOME/.local/bin/codex" \
  --codex-home "$HOME/.codex" \
  --app-server-endpoint unix:///tmp/codex-control.sock \
  <account-id>
```

When live reload is not available, start the conversation through
`codexmarathon run` first. That gives the companion ownership of the process,
which is required before it can stop, deploy the next profile, and resume the
same thread safely.

## Where data lives

By default, CodexMarathon keeps its state under the platform's user
configuration directory. You can override paths for scripts or separate
projects:

| Option | Purpose |
| --- | --- |
| `--state-dir` | Controller state, journal, and diagnostics directory. |
| `--registry` | Non-secret account profile metadata. |
| `--vault` | Protected opaque credential snapshots. |
| `--auth` | The active Codex authentication file used for deployment. |
| `--codex-home` | The existing Codex home and local control state. |
| `--codex` | The existing Codex executable. |

The companion never includes these paths in a release archive. It does not
copy a user's `auth.json` into Git or print its contents in logs.

## Build and test

The normal release builds only the Go companion and does not need Cargo:

```bash
bash scripts/build-release.sh
```

Run the repository checks from the root:

```bash
(cd controller && go test ./...)
(cd controller && go vet ./...)
(cd integration && go test ./...)
python3 scripts/test_package_release.py
python3 scripts/verify_protocol.py --root .
python3 scripts/verify_provenance.py --root .
```

To verify a built archive on an isolated machine:

```bash
python3 scripts/verify_package.py \
  --artifact dist/release/codexmarathon-<version>-linux-<arch>.tar.gz
python3 scripts/clean_machine_check.py \
  --artifact dist/release/codexmarathon-<version>-linux-<arch>.tar.gz
```

The imported Rust runtime and adapter are retained for protocol development
and an explicitly optional self-contained package. They are not needed for a
normal companion build:

```bash
(cd runtime/codexmarathon-adapter && cargo test)
(cd runtime/codex-rs && cargo test --locked -p codexmarathon-runtime)
```

## Release contents

The default archive contains the `codexmarathon` executable, protocol files,
documentation, provenance, and license metadata. It excludes:

- `codex-app-server` and other embedded runtime executables;
- Cargo target directories and developer caches;
- donor repositories;
- `auth.json`, vault snapshots, journals, and controller state.

An embedded runtime can be packaged only by an explicit opt-in:

```bash
CODEXMARATHON_INCLUDE_EMBEDDED_RUNTIME=1 bash scripts/build-release.sh
```

That variant is for diagnostics and offline experiments. It is not the normal
installation for a machine that already has Codex.

## Current scope

The deterministic controller, protocol, packaging, and companion-process tests
run without provider credentials. A real acceptance run still needs an
installed Codex process and usable accounts to prove the complete OAuth,
quota-limit, live-reload, restart, and same-thread resume flow.

See [the features and usage guide](docs/features-and-usage.md) for a command
reference and operating guide, [the companion design](docs/companion-cli.md)
for process ownership and restart details, and [the release checklist](docs/release.md)
for evidence requirements.

## License and provenance

CodexMarathon-specific code is in this repository. The imported Codex-derived
runtime is retained under `runtime/codex-rs/` with its Apache-2.0 license and
notices. Source provenance and patch history are documented in
[`runtime/PROVENANCE.md`](runtime/PROVENANCE.md) and
[`runtime/PATCH_LEDGER.md`](runtime/PATCH_LEDGER.md).
