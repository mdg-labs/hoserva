#!/usr/bin/env bash
# Unit tests for scripts/release/lib.sh — version/channel parsing (Q66)
# and the SHA256SUMS signing helper, the latter against a throwaway
# Ed25519 test key generated and discarded here, never the release's
# real signing key.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "$script_dir/lib.sh"

fail=0
note() { printf 'test-lib: %s\n' "$*" >&2; }
assert_eq() {
  if [ "$1" != "$2" ]; then
    note "FAIL: expected '$2', got '$1' ($3)"
    fail=1
  fi
}

assert_eq "$(hoserva_channel_from_tag v0.1.0)" "stable" "stable tag"
assert_eq "$(hoserva_channel_from_tag v1.2.3)" "stable" "stable tag, multi-digit"
assert_eq "$(hoserva_channel_from_tag v0.1.0-beta.1)" "beta" "beta tag"
assert_eq "$(hoserva_channel_from_tag v0.1.0-beta.12)" "beta" "beta tag, multi-digit"

if hoserva_channel_from_tag 0.1.0 >/dev/null 2>&1; then
  note "FAIL: 'v'-less tag should be refused"
  fail=1
fi
if hoserva_channel_from_tag v0.1.0-rc.1 >/dev/null 2>&1; then
  note "FAIL: an rc tag should be refused — only 'beta' is a recognised channel"
  fail=1
fi

assert_eq "$(hoserva_debian_version_from_tag v0.1.0)" "0.1.0" "stable version"
assert_eq "$(hoserva_debian_version_from_tag v0.1.0-beta.1)" "0.1.0~beta.1" "beta version"

# dpkg orders 0.1.0~beta.1 before 0.1.0 — the whole point of the '~'
# (Debian Policy §5.6.12). Confirmed with dpkg itself when available;
# skipped, not failed, when it isn't (this sandbox's dev host has no
# dpkg tooling at all — CLAUDE.md, see this issue's report).
if command -v dpkg >/dev/null 2>&1; then
  if ! dpkg --compare-versions "0.1.0~beta.1" lt "0.1.0"; then
    note "FAIL: dpkg does not order the beta version before the stable one"
    fail=1
  fi
else
  note "dpkg not installed on this host — skipping the dpkg --compare-versions check (CI has it)"
fi

key_dir="$(mktemp -d)"
cleanup() { rm -rf "$key_dir"; }
trap cleanup EXIT

openssl genpkey -algorithm ed25519 -out "$key_dir/priv.pem" >/dev/null 2>&1
openssl pkey -in "$key_dir/priv.pem" -pubout -out "$key_dir/pub.pem" >/dev/null 2>&1
printf 'deadbeef  hoserva_0.1.0_amd64.deb\n' > "$key_dir/SHA256SUMS"

hoserva_sign_sha256sums "$key_dir/SHA256SUMS" "$key_dir/priv.pem" "$key_dir/SHA256SUMS.sig"

if ! openssl pkeyutl -verify -pubin -inkey "$key_dir/pub.pem" -rawin -in "$key_dir/SHA256SUMS" -sigfile "$key_dir/SHA256SUMS.sig" >/dev/null 2>&1; then
  note "FAIL: SHA256SUMS.sig does not verify against the key that produced it"
  fail=1
fi

echo tampered >> "$key_dir/SHA256SUMS"
if openssl pkeyutl -verify -pubin -inkey "$key_dir/pub.pem" -rawin -in "$key_dir/SHA256SUMS" -sigfile "$key_dir/SHA256SUMS.sig" >/dev/null 2>&1; then
  note "FAIL: SHA256SUMS.sig still verifies after SHA256SUMS was modified"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
