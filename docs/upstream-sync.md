# Upstream Codex synchronization

`runtime/codex-rs` is a pinned copy of the upstream Codex Rust workspace. The
source commit, license, and local Marathon additions are recorded in
[`runtime/PROVENANCE.md`](../runtime/PROVENANCE.md) and
[`runtime/PATCH_LEDGER.md`](../runtime/PATCH_LEDGER.md).

## Automated compatibility gate

The scheduled release workflow does not auto-resolve source conflicts. It
reconstructs the full CodexMarathon delta from the maintained snapshot and the
OpenAI baseline recorded in `.github/upstream-base.txt`, then applies that
delta with Git's three-way merge support. A clean application may proceed to
platform builds and publication; any conflict stops the run before a binary is
produced.

Use the manual workflow dispatch with `port_only: true` for a non-publishing
compatibility check. For a local, reviewable conflict checkout:

```bash
scripts/prepare-upstream-release.sh \
  --upstream-ref rust-vX.Y.Z \
  --destination /tmp/codexmarathon-port \
  --patch-output /tmp/codexmarathon-port-evidence
```

Exit status 2 means a manual port is required. The destination is intentionally
preserved with conflict markers, and the evidence directory contains the exact
generated patches and status report. No release automation may accept the
checkout until the conflicts are resolved and focused tests pass.

## Manual refresh procedure

1. Run the preparation command for the published stable upstream tag and
   resolve the reported conflicts in its separate checkout.
2. Review the complete upstream-to-prepared-tree diff, then replace the
   maintained `runtime/codex-rs` snapshot with the reviewed prepared
   `codex-rs` tree.
3. Update `.github/upstream-base.txt` to the peeled OpenAI commit for that tag,
   and update `runtime/PROVENANCE.md` and `runtime/PATCH_LEDGER.md` in the same
   reviewed change. The refreshed snapshot must contain only the Marathon
   delta relative to the new recorded baseline.
4. Recheck AuthManager reload, safe-boundary enforcement, transport
   invalidation, quota events, and recovery ordering against the new APIs.
5. Run formatting, focused runtime tests, the port-only workflow, and complete
   Linux and Windows release builds.
6. Review the diff for credential leaks and verify that no build cache or local
   account state entered the commit.

Marathon must continue to use Codex's native authorities. It does not shell
out to another executable, maintain a second turn counter, or duplicate a
recovery prompt. If an upstream symbol moves, update the native integration
and its tests at the same time.
