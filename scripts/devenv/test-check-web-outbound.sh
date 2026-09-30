#!/usr/bin/env bash
# Fixture tests for check-web-outbound.sh (Q49): each URL is written into a
# throwaway dist directory and the script must accept or refuse it.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
check="$script_dir/check-web-outbound.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail=0
note() { printf 'test-check-web-outbound: %s\n' "$*" >&2; }

run_check() {
  local dist="$tmp/dist"
  rm -rf "$dist"
  mkdir -p "$dist/assets"
  printf '%s\n' "$1" >"$dist/assets/index.js"
  local rc=0
  "$check" "$dist" >"$tmp/out" 2>"$tmp/err" || rc=$?
  return "$rc"
}

expect_pass() {
  local rc=0
  run_check "$1" || rc=$?
  if [ "$rc" -ne 0 ]; then
    note "FAIL: expected pass, got exit $rc: $1"
    fail=1
  fi
}

expect_fail() {
  local rc=0
  run_check "$1" || rc=$?
  if [ "$rc" -ne 1 ]; then
    note "FAIL: expected exit 1, got $rc: $1"
    fail=1
  elif ! grep -qF "(Q49)" "$tmp/err"; then
    note "FAIL: exit 1 without the Q49 message: $1"
    fail=1
  fi
}

expect_pass 'x="https://cloud.example.com/remote.php/dav/files/user/."'
expect_pass 'x="http://nas.example.org"'
expect_pass 'x="https://host.example/path"'
expect_pass 'x="https://x.test"'
expect_pass 'x="https://y.invalid"'
expect_pass 'x="https://example.net:8443/dav?x=1"'
expect_pass 'x="http://www.w3.org/2000/svg"'
expect_pass 'x="https://react.dev/errors/31"'
expect_pass 'x="http://localhost:5173/"'
expect_pass 'x="https://fb.me/use-check-prop-types"'

expect_fail 'x="https://example.com.evil.io/"'
expect_fail 'x="https://notexample.com/"'
expect_fail 'x="https://api.github.com/"'
expect_fail 'x="https://example.com@evil.io/"'
expect_fail 'x="https://example.com:pw@evil.io/"'
expect_fail 'x="https://evil.io/?u=https://example.com"'
expect_fail 'x="https://cloud.example.com.evil.io/dav"'
expect_fail 'x="https://fb.me/other"'

mixed=$'x="https://cloud.example.com/dav"\ny="https://api.github.com/"'
expect_fail "$mixed"

rc=0
"$check" "$tmp/does-not-exist" >/dev/null 2>&1 || rc=$?
if [ "$rc" -ne 2 ]; then
  note "FAIL: a missing dist directory must fail (exit 2), got $rc"
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "test-check-web-outbound: ok"
