#!/usr/bin/env bash
set -euo pipefail

repo_root="$(pwd)"
tmp_dir="$(mktemp -d)"
ref_name="refs/heads/settlekit-clean-clone-$$"
cleanup() {
  git -C "$repo_root" update-ref -d "$ref_name" >/dev/null 2>&1 || true
  find "$tmp_dir" -depth -delete
}
trap cleanup EXIT

started="$(date +%s)"
if git rev-parse --verify HEAD >/dev/null 2>&1 &&
  test -z "$(git status --porcelain --untracked-files=all)"; then
  commit="$(git rev-parse HEAD)"
  git clone --quiet \
    "$repo_root" "$tmp_dir/checkout"
  test "$(git -C "$tmp_dir/checkout" rev-parse HEAD)" = "$commit"
  source_kind="committed HEAD"
else
  export GIT_INDEX_FILE="$tmp_dir/index"
  git read-tree --empty
  git -c advice.addEmbeddedRepo=false add -A
  tree="$(git write-tree)"
  commit="$(GIT_AUTHOR_NAME='SettleKit local fixture' GIT_AUTHOR_EMAIL='local-fixture@invalid' \
    GIT_COMMITTER_NAME='SettleKit local fixture' GIT_COMMITTER_EMAIL='local-fixture@invalid' \
    git commit-tree "$tree" -m 'Temporary clean-clone verification snapshot')"
  git update-ref "$ref_name" "$commit"
  unset GIT_INDEX_FILE
  git clone --quiet \
    --branch "${ref_name##*/}" "$repo_root" "$tmp_dir/checkout"
  source_kind="temporary snapshot"
fi
(cd "$tmp_dir/checkout" && make anvil-test)
elapsed="$(( $(date +%s) - started ))"
if (( elapsed > 600 )); then
  echo "clean-clone demo exceeded 600 seconds: $elapsed" >&2
  exit 1
fi
echo "clean-clone demo: PASS in ${elapsed}s (${source_kind} ${commit:0:12})"
