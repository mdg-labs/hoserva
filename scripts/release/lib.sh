#!/usr/bin/env bash
# Shared helpers for scripts/release/*.sh (Q66): version/channel parsing
# from a release tag, and signing SHA256SUMS with the release's Ed25519
# key. Sourced, not executed — every caller still validates its own
# arguments rather than trusting this file blindly.

# hoserva_channel_from_tag prints "stable" or "beta" for a tag like
# "v0.1.0" or "v0.1.0-beta.1" — stable versions are GitHub Releases, beta
# versions are pre-releases (Q66). Anything else is refused: a malformed
# tag must stop the release, not silently land on one channel or the
# other.
hoserva_channel_from_tag() {
  local tag="$1"
  if [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-beta\.[0-9]+$ ]]; then
    echo "beta"
  elif [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "stable"
  else
    echo "hoserva_channel_from_tag: '$tag' is not a recognised release tag (want vX.Y.Z or vX.Y.Z-beta.N)" >&2
    return 1
  fi
}

# hoserva_debian_version_from_tag prints the Debian-policy version for a
# release tag: the leading 'v' dropped, and a beta suffix's '-' turned
# into '~' so dpkg's version comparison sorts every beta before the
# stable release it leads up to (Debian Policy §5.6.12).
hoserva_debian_version_from_tag() {
  local tag="$1"
  local version="${tag#v}"
  echo "${version/-beta./~beta.}"
}

# hoserva_verify_tag_ancestry fails unless $1 (a release tag) is an
# ancestor of the branch its channel publishes from: origin/main for a
# stable tag, origin/beta for a beta tag (Q66, doc 12 §6). Without this,
# a `v*` tag pushed to any other commit would still build and sign a
# release — the caller must have fetched origin/main and origin/beta
# first.
hoserva_verify_tag_ancestry() {
  local tag="$1"
  local channel branch
  channel="$(hoserva_channel_from_tag "$tag")" || return 1
  case "$channel" in
    stable) branch="main" ;;
    beta) branch="beta" ;;
  esac
  if ! git merge-base --is-ancestor "$tag" "origin/$branch" 2>/dev/null; then
    echo "hoserva_verify_tag_ancestry: '$tag' (channel: $channel) is not an ancestor of origin/$branch — refusing to build or sign a release from it" >&2
    return 1
  fi
}

# hoserva_sign_sha256sums signs $1 (a SHA256SUMS file) with the Ed25519
# private key at $2 (PEM), writing the detached signature to $3 (Q66:
# "a SHA256SUMS file carrying a detached Ed25519 signature"). openssl's
# pkeyutl signs Ed25519 messages directly (no digest step, unlike every
# other curve/algorithm openssl dgst -sign covers) — confirmed against a
# throwaway test key by scripts/release/test-lib.sh, never the release's
# real key.
hoserva_sign_sha256sums() {
  local sums_file="$1" key_file="$2" sig_file="$3"
  openssl pkeyutl -sign -inkey "$key_file" -rawin -in "$sums_file" -out "$sig_file"
}

# hoserva_ed25519_pubkey_raw writes 32 raw Ed25519 public-key bytes to
# $2 from $1: a PEM file (public or private) or a pubkey.go source file.
# Never prints key material — callers compare the output with cmp(1).
hoserva_ed25519_pubkey_raw() {
  local source="$1" out_file="$2"
  if [ ! -e "$source" ]; then
    echo "hoserva_ed25519_pubkey_raw: no such file: $source" >&2
    return 1
  fi
  if grep -q 'ed25519\.PublicKey' "$source" 2>/dev/null; then
    hoserva_ed25519_pubkey_raw_from_go "$source" "$out_file"
  else
    hoserva_ed25519_pubkey_raw_from_pem "$source" "$out_file"
  fi
}

hoserva_ed25519_pubkey_raw_from_go() {
  local go_file="$1" out_file="$2"
  local count
  count="$(grep -oE '0x[0-9a-fA-F]{2}' "$go_file" | wc -l)"
  if [ "$count" -ne 32 ]; then
    echo "hoserva_ed25519_pubkey_raw_from_go: expected 32 byte literals in $go_file" >&2
    return 1
  fi
  grep -oE '0x[0-9a-fA-F]{2}' "$go_file" | sed 's/0x//' | tr -d '\n' | xxd -r -p >"$out_file"
}

hoserva_ed25519_pubkey_raw_from_pem() {
  local pem_file="$1" out_file="$2"
  local der_file
  der_file="$(mktemp)"
  if openssl pkey -in "$pem_file" -pubin -outform DER -out "$der_file" 2>/dev/null; then
    :
  elif openssl pkey -in "$pem_file" -pubout -outform DER -out "$der_file" 2>/dev/null; then
    :
  else
    rm -f "$der_file"
    echo "hoserva_ed25519_pubkey_raw_from_pem: not a readable Ed25519 PEM: $pem_file" >&2
    return 1
  fi
  tail -c 32 "$der_file" >"$out_file"
  rm -f "$der_file"
}

# hoserva_compare_release_pubkeys compares two pubkey sources ($1 and $2)
# and prints only "match" or "mismatch" on stdout. Exits 0 on match.
hoserva_compare_release_pubkeys() {
  local source_a="$1" source_b="$2"
  local raw_a raw_b
  raw_a="$(mktemp)"
  raw_b="$(mktemp)"
  trap 'rm -f "$raw_a" "$raw_b"' RETURN
  hoserva_ed25519_pubkey_raw "$source_a" "$raw_a" || return 1
  hoserva_ed25519_pubkey_raw "$source_b" "$raw_b" || return 1
  if cmp -s "$raw_a" "$raw_b"; then
    echo "match"
    return 0
  fi
  echo "mismatch"
  return 1
}

# Rotating HOSERVA_RELEASE_SIGNING_KEY (Q66, Q67):
#   1. Generate a new Ed25519 keypair:
#        openssl genpkey -algorithm ed25519 -out new-priv.pem
#   2. Export the public half (keep locally, never commit):
#        openssl pkey -in new-priv.pem -pubout -out release.pub
#   3. Update the HOSERVA_RELEASE_SIGNING_KEY GitHub Actions secret with
#      the contents of new-priv.pem, then delete new-priv.pem from disk.
#   4. Update the HOSERVA_RELEASE_SIGNING_PUBKEY GitHub Actions variable
#      with the contents of release.pub:
#        gh variable set HOSERVA_RELEASE_SIGNING_PUBKEY < release.pub
#   5. Regenerate EmbeddedPublicKey in internal/update/pubkey.go from
#      release.pub — the last 32 bytes of the DER SPKI are the raw key:
#        openssl pkey -in release.pub -pubin -outform DER | tail -c 32 | od -An -tx1
#   6. Ship a release containing the updated EmbeddedPublicKey before any
#      release signed with the new private key is published. Daemons with
#      the old compiled-in key fail closed on signatures from the new key.
