#!/usr/bin/env bash
# Checks the job layout of .github/workflows/release.yml: the release-signing
# key is referenced only by a job that runs none of the build's code, the
# signing job needs the build job and takes its .deb files only from a
# downloaded artifact, and the build job holds neither the key nor a write
# token. The rules live in scripts/release/workflowcheck; its own tests
# cover each rule against a mutated workflow.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
cd "$repo_root"

fail=0
note() { printf 'test-release-workflow: %s\n' "$*" >&2; }

go test ./scripts/release/workflowcheck/ || fail=1
go run ./scripts/release/workflowcheck .github/workflows/release.yml || {
  note "FAIL: .github/workflows/release.yml breaks the job layout"
  fail=1
}

for f in "$script_dir/lib.sh" "$script_dir/publish-release.sh" "$script_dir/compare-release-pubkey.sh" "$script_dir/stage-release-artifacts.sh"; do
  if grep -nE '^[^#]*(^|[[:space:];&|(`])openssl[[:space:]]' "$f" >&2; then
    note "FAIL: $f calls openssl through PATH"
    fail=1
  fi
done
if grep -nE '^[^#]*(^|[[:space:];&|(`])openssl[[:space:]]' "$repo_root/.github/workflows/release.yml" >&2; then
  note "FAIL: release.yml calls openssl through PATH"
  fail=1
fi

[ "$fail" -eq 0 ] || exit 1
note "ok"
