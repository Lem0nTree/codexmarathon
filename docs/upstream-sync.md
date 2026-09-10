# Upstream Codex synchronization

`runtime/codex-rs` is a pinned copy of the upstream Codex Rust workspace. The
source commit, license, and local Marathon additions are recorded in
[`runtime/PROVENANCE.md`](../runtime/PROVENANCE.md) and
[`runtime/PATCH_LEDGER.md`](../runtime/PATCH_LEDGER.md).

## Refresh procedure

1. Import the new upstream Rust workspace into a separate review branch.
2. Reapply the native Marathon app-server, CLI, TUI, authentication, and
   status-line changes.
3. Recheck AuthManager reload, safe-boundary enforcement, transport
   invalidation, quota events, and recovery ordering against the new APIs.
4. Run formatting, focused runtime tests, and a release build of `codex-cli`.
5. Review the diff for credential leaks and verify that no build cache or local
   account state entered the commit.

Marathon must continue to use Codex's native authorities. It does not shell
out to another executable, maintain a second turn counter, or duplicate a
recovery prompt. If an upstream symbol moves, update the native integration
and its tests at the same time.
