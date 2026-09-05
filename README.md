# CodexMarathon

Keep your Codex work moving across multiple accounts.

CodexMarathon is a companion CLI for the Codex CLI already installed on your
computer. It keeps account profiles, watches usage, and safely moves a long
conversation to another account when the current account reaches a limit.

## Why use it?

- Keep work, personal, and project accounts in one place.
- Switch accounts without manually copying credential files.
- Continue the same conversation after a usage limit.
- Choose an account yourself or let the usage policy choose one.
- Keep your existing Codex version, settings, plugins, and sessions.
- See account health and transition status without exposing tokens.
- Install a small companion beside Codex.

## Example

You use one account for work and another for personal projects:

```bash
codexmarathon accounts list
# work       Work account
# personal   Personal account

codexmarathon run --codex codex --codex-dir ~/projects/website
```

If the active account reaches its limit, CodexMarathon can select the other
eligible account and continue the same conversation. You can also switch
manually:

```bash
codexmarathon switch work
```

The change happens at a safe point in the current turn. CodexMarathon uses a
live account reload when the installed Codex supports it. Otherwise, it
cleanly restarts a process it launched and resumes the same thread.

## Install

Install Codex first and check that it works:

```bash
codex --version
```

Download the latest archive from the
[GitHub releases page](https://github.com/Lem0nTree/codexmarathon/releases),
then install it:

```bash
tar -xzf codexmarathon-<version>-linux-<arch>.tar.gz
cd codexmarathon-<version>-linux-<arch>
install -m 0755 codexmarathon "$HOME/.local/bin/codexmarathon"
codexmarathon init
```

Use `linux-x86_64` for amd64 machines and `linux-aarch64` for ARM64 machines.
CodexMarathon works alongside your existing Codex installation.

## Quick start

```bash
codexmarathon status --json
codexmarathon accounts list
codexmarathon run --codex codex
```

If Codex is outside `PATH`, pass its full path:

```bash
codexmarathon run --codex "$HOME/.local/bin/codex"
```

Use `--codex-dir` for a project and `--codex-home` for a non-default Codex
home. Use `--attach-only` when you want to connect to an existing compatible
Codex session instead of launching one.

## Commands

```text
codexmarathon init
codexmarathon status [--json]
codexmarathon run [options]
codexmarathon accounts list
codexmarathon accounts status [account-id]
codexmarathon accounts login [options]
codexmarathon accounts activate <account-id>
codexmarathon accounts use <account-id>
codexmarathon accounts rename --name <alias> <account-id>
codexmarathon accounts remove <account-id>
codexmarathon switch <account-id>
```

Use `codexmarathon accounts list` to find account IDs. Login and refresh use
the authentication setup available in your Codex installation; the detailed
options are in [Features and usage](docs/features-and-usage.md).

## Safe handoff

CodexMarathon waits for a safe turn boundary before changing accounts. It
reloads credentials through Codex when possible. If a restart is needed, it
only controls a Codex process started by `codexmarathon run`, deploys the next
profile atomically, resumes the same conversation, and verifies the target
account. It never interrupts an unrelated Codex process or creates a duplicate
resume prompt.

## Security

Credential snapshots are treated as private opaque data. They are kept in the
companion's protected state, excluded from logs and release files, and never
printed by status commands. Account metadata and transition records contain no
access or refresh tokens.

## Build and test

```bash
bash scripts/build-release.sh
(cd controller && go test ./...)
(cd controller && go vet ./...)
(cd integration && go test ./...)
python3 scripts/test_package_release.py
```

See [Build and verification](docs/build-test.md) for the complete check list.

## Documentation

- [Features and usage](docs/features-and-usage.md)
- [Companion design](docs/companion-cli.md)
- [Architecture](docs/architecture.md)
- [Build and verification](docs/build-test.md)
- [Release checklist](docs/release.md)
- [Project plan](PLAN.md)

CodexMarathon-specific code is provided in this repository. Imported
Codex-derived source and its notices are documented in
[runtime provenance](runtime/PROVENANCE.md).
