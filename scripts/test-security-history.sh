#!/usr/bin/env bash
# Contract tests for scripts/security-history.sh: a fixture git repository
# whose commits fix a security-labelled issue, a plain issue and nothing,
# read through a fake `gh` on PATH — never the live API.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
HISTORY="$script_dir/security-history.sh"

fail=0
note() { printf 'test-security-history: %s\n' "$*" >&2; }
assert_contains() {
  if ! grep -qF -- "$2" "$1"; then
    note "FAIL: output does not contain '$2' ($3): $(cat "$1")"
    fail=1
  fi
}
assert_not_contains() {
  if grep -qF -- "$2" "$1"; then
    note "FAIL: output unexpectedly contains '$2' ($3): $(cat "$1")"
    fail=1
  fi
}
assert_empty() {
  if [ -s "$1" ]; then
    note "FAIL: expected no output ($2): $(cat "$1")"
    fail=1
  fi
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

export GH_REPO="test-owner/test-repo"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.test
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.test

mock_bin="$work/mock-bin"
mkdir -p "$mock_bin"
cat >"$mock_bin/gh" <<'MOCK_EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" = "api" ] || { echo "mock gh: unexpected invocation $*" >&2; exit 1; }
[ -z "${MOCK_GH_FAIL:-}" ] || { echo "mock gh: forced failure" >&2; exit 1; }
path=""
shift
while [ $# -gt 0 ]; do
  case "$1" in
    -f|-F|--jq) shift 2 ;;
    --*) shift ;;
    *) path="$1"; shift ;;
  esac
done
case "$path" in
  "repos/test-owner/test-repo/issues?state=all&labels=security&per_page=100&page=1")
    cat <<'JSON'
[{"number":656,"title":"Mover follows a symlink as root","labels":[{"name":"security"}]},
 {"number":665,"title":"Config file readable by every local user","labels":[{"name":"security"}]},
 {"number":684,"title":"Catch-all branch list","labels":[{"name":"security"}]}]
JSON
    ;;
  *)
    echo "mock gh: unexpected invocation: $path" >&2
    exit 1
    ;;
esac
MOCK_EOF
chmod +x "$mock_bin/gh"
PATH="$mock_bin:$PATH"
export PATH

repo="$work/repo"
git init -q "$repo"
cd "$repo"
mkdir -p internal/pool internal/config web
commit() {
  local path=$1 message=$2
  printf '%s\n' "$message" >>"$path"
  git add "$path"
  git commit -q -m "$message"
}

commit internal/pool/copy.go "$(printf 'fix(pool): resolve each path component without following symlinks\n\nThe copy walks the target one component at a time.\n\nFixes #656\n\nSigned-off-by: t <t@example.test>')"
commit internal/pool/other.go "$(printf 'feat(pool): list the branches\n\nFixes #700\n\nSigned-off-by: t <t@example.test>')"
commit internal/config/upsmon.go "$(printf 'fix(config): write the UPS config 0600\n\nFixes #665\n\nSigned-off-by: t <t@example.test>')"
commit internal/pool/branches.go "$(printf 'fix(pool): keep the catch-all on the plain branches\n\nFixes mdg-labs/hoserva#684\n\nSigned-off-by: t <t@example.test>')"
commit web/page.ts "$(printf 'feat(web): a page\n\nThis sentence ends in Fixes #656\n\nSigned-off-by: t <t@example.test>')"
commit internal/pool/misc.go "$(printf 'chore(pool): tidy\n\nNo trailer here.')"

# --- no arguments is refused -------------------------------------------
set +e
"$HISTORY" >"$work/none.out" 2>"$work/none.err"; rc=$?
set -e
if [ "$rc" -ne 2 ]; then
  note "FAIL: no arguments must exit 2 (got $rc)"
  fail=1
fi
assert_contains "$work/none.err" "usage" "no arguments prints the usage"
assert_empty "$work/none.out" "no arguments prints nothing on stdout"

# --- a directory with security fixes lists exactly those ----------------
"$HISTORY" internal/pool/ >"$work/pool.out"
assert_contains "$work/pool.out" "resolve each path component without following symlinks" "the labelled fix is listed with its subject"
assert_contains "$work/pool.out" "fixes #656: Mover follows a symlink as root" "the issue number and title are shown"
assert_contains "$work/pool.out" "keep the catch-all on the plain branches" "the cross-repository Fixes form is matched"
assert_not_contains "$work/pool.out" "list the branches" "a commit fixing an unlabelled issue is left out"
assert_not_contains "$work/pool.out" "tidy" "a commit with no trailer is left out"
assert_not_contains "$work/pool.out" "UPS config" "a commit outside the path is left out"
if [ "$(wc -l <"$work/pool.out")" -ne 2 ]; then
  note "FAIL: internal/pool/ should list two commits: $(cat "$work/pool.out")"
  fail=1
fi
sha=$(git log -1 --format=%h --grep='resolve each path component')
assert_contains "$work/pool.out" "$sha " "the line starts with the commit's short sha"

# --- a single file, and several paths -----------------------------------
"$HISTORY" internal/pool/copy.go >"$work/file.out"
assert_contains "$work/file.out" "fixes #656" "a single file path finds its fix"
assert_not_contains "$work/file.out" "catch-all" "a single file path excludes other files' fixes"

"$HISTORY" internal/config/ internal/pool/copy.go >"$work/multi.out"
assert_contains "$work/multi.out" "fixes #665" "a second path adds its fix"
assert_contains "$work/multi.out" "fixes #656" "the first path's fix stays"

# --- a sentence ending in `Fixes #n` is not a trailer; a path with no fix prints nothing
"$HISTORY" web/ >"$work/web.out"
assert_empty "$work/web.out" "a path with no security fix prints nothing"

# --- the cap -------------------------------------------------------------
SECURITY_HISTORY_MAX=1 "$HISTORY" internal/ >"$work/cap.out"
if [ "$(wc -l <"$work/cap.out")" -ne 2 ]; then
  note "FAIL: a cap of 1 over three fixes should print one commit and one summary line: $(cat "$work/cap.out")"
  fail=1
fi
assert_contains "$work/cap.out" "... and 2 more" "the capped output says how many are left"

# --- an unreadable history is an error, never an empty answer -----------
set +e
MOCK_GH_FAIL=1 "$HISTORY" internal/pool/ >"$work/fail.out" 2>"$work/fail.err"; rc=$?
set -e
if [ "$rc" -ne 2 ]; then
  note "FAIL: a GitHub failure must exit 2 (got $rc)"
  fail=1
fi
assert_empty "$work/fail.out" "a GitHub failure prints nothing on stdout"
assert_contains "$work/fail.err" "could not list the issues labelled security" "the failure is named"

set +e
(cd "$work" && "$HISTORY" internal/pool/ >"$work/nogit.out" 2>"$work/nogit.err"); rc=$?
set -e
if [ "$rc" -ne 2 ]; then
  note "FAIL: a directory that is not a git checkout must exit 2 (got $rc)"
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi
note "PASS"
