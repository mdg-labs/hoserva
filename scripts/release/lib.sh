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
