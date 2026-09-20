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

# hoserva_verify_tag_ancestry: a throwaway git repo with a bare "origin"
# stands in for the real GitHub remote, never the project's own repo.
git_dir="$(mktemp -d)"
trap 'cleanup; rm -rf "$git_dir"' EXIT

bare="$git_dir/origin.git"
git init --quiet --bare "$bare"

repo="$git_dir/repo"
git init --quiet "$repo"
git -C "$repo" config user.email test@example.com
git -C "$repo" config user.name test
git -C "$repo" remote add origin "$bare"

git -C "$repo" checkout --quiet -b main
echo one >"$repo/f"
git -C "$repo" add f
git -C "$repo" commit --quiet -m one
git -C "$repo" tag v0.1.0
git -C "$repo" push --quiet origin main --tags

git -C "$repo" checkout --quiet -b beta
echo two >>"$repo/f"
git -C "$repo" add f
git -C "$repo" commit --quiet -m two
git -C "$repo" tag v0.1.0-beta.1
git -C "$repo" push --quiet origin beta --tags

# a stable tag cut from beta's extra commit must be refused: it is not
# an ancestor of origin/main even though it matches the stable pattern.
git -C "$repo" tag v9.9.9
git -C "$repo" push --quiet origin --tags

git -C "$repo" fetch --quiet origin main beta

if ! (cd "$repo" && hoserva_verify_tag_ancestry v0.1.0) >/dev/null 2>&1; then
  note "FAIL: v0.1.0 should be an ancestor of origin/main"
  fail=1
fi
if ! (cd "$repo" && hoserva_verify_tag_ancestry v0.1.0-beta.1) >/dev/null 2>&1; then
  note "FAIL: v0.1.0-beta.1 should be an ancestor of origin/beta"
  fail=1
fi
if (cd "$repo" && hoserva_verify_tag_ancestry v9.9.9) >/dev/null 2>&1; then
  note "FAIL: v9.9.9 (only on beta) should not be accepted as a stable tag from main"
  fail=1
fi

write_test_pubkey_go() {
  local pem_file="$1" go_file="$2"
  {
    echo 'package update'
    echo 'import "crypto/ed25519"'
    echo 'var EmbeddedPublicKey = ed25519.PublicKey{'
    openssl pkey -in "$pem_file" -pubin -outform DER | tail -c 32 | od -An -tx1 | tr -s ' \n' ' ' |
      sed 's/ /, 0x/g' | sed 's/^/0x/' | fold -s -w 72 |
      sed 's/$/,/' | sed '$ s/,$//'
    echo '}'
  } >"$go_file"
}

match_key_dir="$(mktemp -d)"
other_key_dir="$(mktemp -d)"
trap 'cleanup; rm -rf "$git_dir" "$match_key_dir" "$other_key_dir"' EXIT

openssl genpkey -algorithm ed25519 -out "$match_key_dir/priv.pem" >/dev/null 2>&1
openssl pkey -in "$match_key_dir/priv.pem" -pubout -out "$match_key_dir/pub.pem" >/dev/null 2>&1
write_test_pubkey_go "$match_key_dir/pub.pem" "$match_key_dir/pubkey.go"

openssl genpkey -algorithm ed25519 -out "$other_key_dir/priv.pem" >/dev/null 2>&1
openssl pkey -in "$other_key_dir/priv.pem" -pubout -out "$other_key_dir/pub.pem" >/dev/null 2>&1
write_test_pubkey_go "$other_key_dir/pub.pem" "$other_key_dir/pubkey.go"

if [ "$(hoserva_compare_release_pubkeys "$match_key_dir/pub.pem" "$match_key_dir/pubkey.go")" != match ]; then
  note "FAIL: matching PEM and pubkey.go should compare as match"
  fail=1
fi
if [ "$(hoserva_compare_release_pubkeys "$match_key_dir/priv.pem" "$match_key_dir/pubkey.go")" != match ]; then
  note "FAIL: matching private PEM and pubkey.go should compare as match"
  fail=1
fi
if [ "$(hoserva_compare_release_pubkeys "$match_key_dir/pubkey.go" "$other_key_dir/pubkey.go")" = match ]; then
  note "FAIL: different pubkey.go sources should not compare as match"
  fail=1
fi
if hoserva_compare_release_pubkeys "$match_key_dir/pubkey.go" /no/such/file >/dev/null 2>&1; then
  note "FAIL: missing input should not compare successfully"
  fail=1
fi

# A non-Ed25519 PEM must not be accepted by taking its last 32 DER bytes
# (X25519 SPKI is the same length as Ed25519; RSA is longer). Either
# would otherwise let a rotated signing key pass the pubkey comparison.
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$match_key_dir/rsa.pem" >/dev/null 2>&1
if hoserva_ed25519_pubkey_raw_from_pem "$match_key_dir/rsa.pem" "$match_key_dir/rsa.raw" >/dev/null 2>&1; then
  note "FAIL: RSA PEM should be refused as a release public key"
  fail=1
fi
openssl genpkey -algorithm X25519 -out "$match_key_dir/x25519.pem" >/dev/null 2>&1
if hoserva_ed25519_pubkey_raw_from_pem "$match_key_dir/x25519.pem" "$match_key_dir/x25519.raw" >/dev/null 2>&1; then
  note "FAIL: X25519 PEM should be refused as a release public key"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
