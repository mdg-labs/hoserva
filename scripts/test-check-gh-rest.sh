#!/usr/bin/env bash
# Unit tests for scripts/check-gh-rest.sh (issue #410): one fixture per
# forbidden pattern in scripts/testdata/gh-rest-check/, run against a
# scratch copy of the checker so the real .claude/ and scripts/ trees are
# never touched.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
fixtures_dir="$script_dir/testdata/gh-rest-check"

fail=0
note() { printf 'test-check-gh-rest: %s\n' "$*" >&2; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/scripts" "$work/.claude"
cp "$script_dir/check-gh-rest.sh" "$work/scripts/check-gh-rest.sh"
chmod +x "$work/scripts/check-gh-rest.sh"

run_check() {
  "$work/scripts/check-gh-rest.sh"
}

# A clean tree (just the fixture that uses gh-rest.sh itself) passes.
cp "$fixtures_dir/12-clean.md" "$work/.claude/fixture.md"
if ! run_check >"$work/out.log" 2>&1; then
  note "FAIL: a clean tree should pass"
  cat "$work/out.log" >&2
  fail=1
fi
rm -f "$work/.claude/fixture.md"

# Every other fixture is a forbidden pattern on its own and must fail —
# then removing it must make the tree pass again.
for fixture in "$fixtures_dir"/0*.md "$fixtures_dir"/10-*.md; do
  name="$(basename "$fixture")"
  [ "$name" = "12-clean.md" ] && continue
  cp "$fixture" "$work/.claude/fixture.md"
  if run_check >"$work/out.log" 2>&1; then
    note "FAIL: $name should have failed the check"
    fail=1
  elif ! grep -q 'forbidden pattern' "$work/out.log"; then
    note "FAIL: $name failed for the wrong reason: $(cat "$work/out.log")"
    fail=1
  fi
  rm -f "$work/.claude/fixture.md"
done

# The allowlist: the same forbidden line passes once its file:substring is
# in scripts/gh-rest-allowlist.txt, and still fails for a *different* file.
cp "$fixtures_dir/11-allowlisted-prohibition.md" "$work/.claude/prohibition.md"
if run_check >"$work/out.log" 2>&1; then
  note "FAIL: an unlisted prohibition line should still fail"
  fail=1
fi
cat >"$work/scripts/gh-rest-allowlist.txt" <<'EOF'
# Test allowlist: this line only forbids the command by naming it.
.claude/prohibition.md::Never close an issue by hand
EOF
if ! run_check >"$work/out.log" 2>&1; then
  note "FAIL: the allowlisted file:substring should now pass"
  cat "$work/out.log" >&2
  fail=1
fi
cp "$fixtures_dir/11-allowlisted-prohibition.md" "$work/.claude/other.md"
if run_check >"$work/out.log" 2>&1; then
  note "FAIL: the allowlist entry must not cover a different file"
  fail=1
fi
rm -f "$work/.claude/prohibition.md" "$work/.claude/other.md" "$work/scripts/gh-rest-allowlist.txt"

# --paginate and --search are flagged wherever they appear, not just in a
# gh invocation, since the acceptance criteria ban them outright.
cp "$fixtures_dir/10-flag-paginate.md" "$work/.claude/fixture.md"
if run_check >"$work/out.log" 2>&1; then
  note "FAIL: --paginate should fail the check"
  fail=1
fi
rm -f "$work/.claude/fixture.md"

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
