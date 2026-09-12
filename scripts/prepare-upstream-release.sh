#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: prepare-upstream-release.sh --upstream-ref REF --destination DIR [options]

Reconstruct the CodexMarathon patch from the pinned upstream baseline and apply
it to REF in a fresh OpenAI Codex checkout. The command deliberately leaves a
conflicted checkout behind and exits non-zero when a port requires review.

Options:
  --snapshot DIR       Patched codex-rs snapshot (default: runtime/codex-rs)
  --base-ref REF       Override the baseline in .github/upstream-base.txt
  --source-url URL     Upstream repository (default: openai/codex)
  --patch-output DIR   Write patch/provenance files here (default: DIR-evidence)
  -h, --help           Show this help
EOF
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
snapshot_dir="${repo_root}/runtime/codex-rs"
base_ref="$(tr -d '[:space:]' < "$repo_root/.github/upstream-base.txt")"
source_url="https://github.com/openai/codex.git"
upstream_ref=""
destination=""
patch_output=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --snapshot)
      snapshot_dir="${2:?--snapshot requires a value}"
      shift 2
      ;;
    --base-ref)
      base_ref="${2:?--base-ref requires a value}"
      shift 2
      ;;
    --source-url)
      source_url="${2:?--source-url requires a value}"
      shift 2
      ;;
    --upstream-ref)
      upstream_ref="${2:?--upstream-ref requires a value}"
      shift 2
      ;;
    --destination)
      destination="${2:?--destination requires a value}"
      shift 2
      ;;
    --patch-output)
      patch_output="${2:?--patch-output requires a value}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unexpected argument: $1" >&2
      usage >&2
      exit 64
      ;;
  esac
done

if [[ -z "$upstream_ref" || -z "$destination" ]]; then
  usage >&2
  exit 64
fi
if [[ ! -d "$snapshot_dir" || ! -f "$snapshot_dir/Cargo.toml" ]]; then
  echo "Snapshot is not a codex-rs workspace: $snapshot_dir" >&2
  exit 64
fi
if [[ -e "$destination" ]] && [[ -n "$(find "$destination" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" ]]; then
  echo "Destination must not exist or must be empty: $destination" >&2
  exit 64
fi
if ! command -v rsync >/dev/null 2>&1; then
  echo "rsync is required to reconstruct the customization patch" >&2
  exit 69
fi

patch_output="${patch_output:-${destination}-evidence}"
mkdir -p "$destination" "$patch_output"
destination="$(cd "$destination" && pwd)"
patch_output="$(cd "$patch_output" && pwd)"
snapshot_dir="$(cd "$snapshot_dir" && pwd)"

combined_patch="$patch_output/codexmarathon-customizations.patch"
additions_patch="$patch_output/codexmarathon-additions.patch"
changes_patch="$patch_output/codexmarathon-changes.patch"
status_file="$patch_output/port-status.txt"
metadata_file="$patch_output/source-metadata.env"

git -C "$destination" init --quiet
git -C "$destination" remote add origin "$source_url"
git -C "$destination" fetch --quiet --no-tags --depth=1 origin "$base_ref"
base_commit="$(git -C "$destination" rev-parse 'FETCH_HEAD^{commit}')"
git -C "$destination" checkout --quiet --detach "$base_commit"

# Recreate the maintained delta instead of trusting a hand-maintained patch.
# The complete vendored snapshot is the reviewed customization source of truth.
rsync -a --delete --exclude target/ "$snapshot_dir/" "$destination/codex-rs/"
git -C "$destination" add -A -- codex-rs
git -C "$destination" diff --cached --binary --full-index --no-ext-diff -- codex-rs \
  > "$combined_patch"
git -C "$destination" diff --cached --binary --full-index --no-ext-diff \
  --diff-filter=A -- codex-rs > "$additions_patch"
git -C "$destination" diff --cached --binary --full-index --no-ext-diff \
  --diff-filter=CDMRTUXB -- codex-rs > "$changes_patch"

if [[ ! -s "$combined_patch" ]]; then
  echo "The maintained snapshot has no delta from the configured baseline" >&2
  exit 65
fi

git -C "$destination" reset --quiet
# This repository was created by this command and contains only disposable
# reconstruction state. Remove snapshot-only paths before checking out target.
git -C "$destination" clean -ffdx -- codex-rs >/dev/null
git -C "$destination" fetch --quiet --no-tags --depth=1 origin "$upstream_ref"
upstream_commit="$(git -C "$destination" rev-parse 'FETCH_HEAD^{commit}')"
git -C "$destination" checkout --quiet --force --detach "$upstream_commit"

apply_status=0
if [[ -s "$additions_patch" ]]; then
  git -C "$destination" apply --index "$additions_patch" || apply_status=$?
fi
if [[ "$apply_status" -eq 0 && -s "$changes_patch" ]]; then
  git -C "$destination" apply --3way --index "$changes_patch" || apply_status=$?
fi

if [[ "$apply_status" -ne 0 ]]; then
  {
    echo "CodexMarathon does not apply cleanly to $upstream_ref ($upstream_commit)."
    echo "No binary or release was produced. Resolve the port in: $destination"
    echo
    echo "Conflicted paths:"
    git -C "$destination" diff --name-only --diff-filter=U
    echo
    echo "Checkout status:"
    git -C "$destination" status --short
  } | tee "$status_file" >&2
  exit 2
fi

git -C "$destination" diff --cached --check
source_tree="$(git -C "$destination" write-tree)"
customization_commit="${CODEXMARATHON_CUSTOMIZATION_COMMIT:-$(git -C "$repo_root" rev-parse HEAD)}"
patch_sha256="$(sha256sum "$combined_patch" | awk '{print $1}')"

cat > "$metadata_file" <<EOF
UPSTREAM_REF=$upstream_ref
UPSTREAM_COMMIT=$upstream_commit
UPSTREAM_BASE_COMMIT=$base_commit
CUSTOMIZATION_COMMIT=$customization_commit
CUSTOMIZATION_PATCH_SHA256=$patch_sha256
PREPARED_SOURCE_TREE=$source_tree
EOF

{
  echo "CodexMarathon applied cleanly to $upstream_ref ($upstream_commit)."
  echo "Prepared source tree: $source_tree"
  echo "Customization patch sha256: $patch_sha256"
} | tee "$status_file"
