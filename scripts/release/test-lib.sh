#!/usr/bin/env bash
# Unit tests for scripts/release/lib.sh — version/channel parsing (Q66)
# and the SHA256SUMS signing helper, the latter against a throwaway
# Ed25519 test key generated and discarded here, never the release's
# real signing key.
set -euo pipefail

# Throwaway repos must not pick up the developer's signing config (tag.gpgsign, commit.gpgsign).
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

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

git -C "$repo" checkout --quiet -b dev
echo two >>"$repo/f"
git -C "$repo" add f
git -C "$repo" commit --quiet -m two
git -C "$repo" tag v0.1.0-beta.1
git -C "$repo" push --quiet origin dev --tags

# a stable tag cut from dev's extra commit must be refused: it is not
# an ancestor of origin/main even though it matches the stable pattern.
git -C "$repo" tag v9.9.9
git -C "$repo" push --quiet origin --tags

git -C "$repo" fetch --quiet origin main dev

if ! (cd "$repo" && hoserva_verify_tag_ancestry v0.1.0) >/dev/null 2>&1; then
  note "FAIL: v0.1.0 should be an ancestor of origin/main"
  fail=1
fi
if ! (cd "$repo" && hoserva_verify_tag_ancestry v0.1.0-beta.1) >/dev/null 2>&1; then
  note "FAIL: v0.1.0-beta.1 should be an ancestor of origin/dev"
  fail=1
fi
if (cd "$repo" && hoserva_verify_tag_ancestry v9.9.9) >/dev/null 2>&1; then
  note "FAIL: v9.9.9 (only on dev) should not be accepted as a stable tag from main"
  fail=1
fi

# hoserva_verify_docs_snapshot: throwaway fixture trees, never the real
# site/. A stable tag needs site/versioned_docs/version-X.Y/ and an X.Y
# entry in site/versions.json; a beta tag needs neither.
docs_dir="$(mktemp -d)"
trap 'cleanup; rm -rf "$git_dir" "$docs_dir"' EXIT

docs_ok() { hoserva_verify_docs_snapshot "$1" "$2" >/dev/null 2>&1; }
docs_tree() {
  local root="$docs_dir/$1" versions="$2"
  mkdir -p "$root/site"
  [ -z "$3" ] || mkdir -p "$root/site/versioned_docs/version-$3"
  [ -z "$versions" ] || printf '%s' "$versions" >"$root/site/versions.json"
  echo "$root"
}

full="$(docs_tree full '["0.2","0.1"]' 0.1)"
if ! docs_ok v0.1.0 "$full"; then
  note "FAIL: v0.1.0 with its snapshot and versions.json entry should pass"
  fail=1
fi
if ! docs_ok v0.1.7 "$full"; then
  note "FAIL: a patch tag v0.1.7 should pass on the 0.1 snapshot"
  fail=1
fi
if ! docs_ok v0.1.0-beta.1 "$docs_dir/no-such-tree"; then
  note "FAIL: a beta tag should pass without any snapshot or site/"
  fail=1
fi
if docs_ok v0.2.0 "$full"; then
  note "FAIL: v0.2.0 should be refused: its directory is missing even though versions.json lists 0.2"
  fail=1
fi
if docs_ok v0.10.0 "$full"; then
  note "FAIL: v0.10.0 should be refused: 0.1 is not 0.10"
  fail=1
fi

nodir="$(docs_tree nodir '["0.1"]' '')"
if docs_ok v0.1.0 "$nodir"; then
  note "FAIL: a stable tag without site/versioned_docs/version-0.1/ should be refused"
  fail=1
fi
nolist="$(docs_tree nolist '["0.2"]' 0.1)"
if docs_ok v0.1.0 "$nolist"; then
  note "FAIL: a stable tag whose minor is not in versions.json should be refused"
  fail=1
fi
nofile="$(docs_tree nofile '' 0.1)"
if docs_ok v0.1.0 "$nofile"; then
  note "FAIL: a stable tag with no versions.json should be refused"
  fail=1
fi
nosite="$docs_dir/nosite"
mkdir -p "$nosite"
if docs_ok v0.1.0 "$nosite"; then
  note "FAIL: a stable tag with no site/ at all should be refused"
  fail=1
fi
for bad_json in '{not json' '{"0.1":true}' '{"latest":"0.1"}' '"0.1"' '[]' '[0.1]'; do
  badjson="$(docs_tree "bad$RANDOM" "$bad_json" 0.1)"
  if docs_ok v0.1.0 "$badjson"; then
    note "FAIL: versions.json '$bad_json' should be refused"
    fail=1
  fi
done
if docs_ok v0.1 "$full"; then
  note "FAIL: an unrecognised tag should be refused"
  fail=1
fi
if docs_ok v0.1.0-rc.1 "$full"; then
  note "FAIL: an rc tag should be refused, not treated as beta"
  fail=1
fi

docs_err="$(hoserva_verify_docs_snapshot v0.3.1 "$nosite" 2>&1 || true)"
case "$docs_err" in
  *site/versioned_docs/version-0.3/*"npx docusaurus docs:version 0.3"*) ;;
  *)
    note "FAIL: the refusal should name the missing directory and the docs:version command, got: $docs_err"
    fail=1
    ;;
esac

# no jq on PATH: the check must refuse, not skip the versions.json test.
nojq_bin="$docs_dir/nojq-bin"
mkdir -p "$nojq_bin"
for tool in bash dirname; do
  ln -s "$(command -v "$tool")" "$nojq_bin/$tool"
done
nojq_rc=0
nojq_err="$(PATH="$nojq_bin" hoserva_verify_docs_snapshot v0.1.0 "$full" 2>&1)" || nojq_rc=$?
if [ "$nojq_rc" -eq 0 ]; then
  note "FAIL: a stable tag should be refused when jq is unavailable, but the check passed"
  fail=1
fi
case "$nojq_err" in
  *"jq is not installed"*) ;;
  *)
    note "FAIL: a stable tag should be refused with a jq message when jq is unavailable, got: $nojq_err"
    fail=1
    ;;
esac

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
trap 'cleanup; rm -rf "$git_dir" "$docs_dir" "$match_key_dir" "$other_key_dir"' EXIT

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
