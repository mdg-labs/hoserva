#!/usr/bin/env bash
# Publishes one release tag to GitHub Releases (Q66): stable tags as a
# release, beta tags as a pre-release, with a SHA256SUMS file over both
# architectures' .deb carrying a detached Ed25519 signature. Run only
# from release.yml, after scripts/release/build-deb.sh has produced both
# architectures' .deb into artifacts_dir.
#
# Q66 also has this update the signed release index on the project
# site. This emits release-index-entry.json as a release asset: a
# self-contained fragment (tag, channel, version, per-arch asset URL
# and checksum) that .github/workflows/pages.yml fetches from every
# past release (scripts/release/fetch-release-index-entries.sh) and
# assembles into the permanent URL
# https://hoserva.dev/releases/index.json.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/lib.sh
source "$script_dir/lib.sh"

usage() {
  echo "usage: $0 <git-tag> <artifacts-dir> <ed25519-private-key.pem>" >&2
  exit 1
}

[ $# -eq 3 ] || usage
tag="$1"
artifacts_dir="$2"
key_file="$3"

channel="$(hoserva_channel_from_tag "$tag")"
version="$(hoserva_debian_version_from_tag "$tag")"
repo="${GITHUB_REPOSITORY:-mdg-labs/hoserva}"

amd64_deb="$artifacts_dir/hoserva_${version}_amd64.deb"
arm64_deb="$artifacts_dir/hoserva_${version}_arm64.deb"
for f in "$amd64_deb" "$arm64_deb"; do
  [ -f "$f" ] || {
    echo "publish-release.sh: missing built package $f" >&2
    exit 1
  }
done

(cd "$artifacts_dir" && sha256sum "$(basename "$amd64_deb")" "$(basename "$arm64_deb")") >"$artifacts_dir/SHA256SUMS"
hoserva_sign_sha256sums "$artifacts_dir/SHA256SUMS" "$key_file" "$artifacts_dir/SHA256SUMS.sig"

amd64_sum="$(sha256sum "$amd64_deb" | cut -d' ' -f1)"
arm64_sum="$(sha256sum "$arm64_deb" | cut -d' ' -f1)"
base_url="https://github.com/${repo}/releases/download/${tag}"

cat >"$artifacts_dir/release-index-entry.json" <<JSON
{
  "tag": "${tag}",
  "version": "${version}",
  "channel": "${channel}",
  "assets": {
    "amd64": {"url": "${base_url}/hoserva_${version}_amd64.deb", "sha256": "${amd64_sum}"},
    "arm64": {"url": "${base_url}/hoserva_${version}_arm64.deb", "sha256": "${arm64_sum}"}
  }
}
JSON

gh_args=(release create "$tag"
  "$amd64_deb" "$arm64_deb"
  "$artifacts_dir/SHA256SUMS" "$artifacts_dir/SHA256SUMS.sig" "$artifacts_dir/release-index-entry.json"
  --repo "$repo"
  --title "hoserva $version"
  --notes "SHA256SUMS carries a detached Ed25519 signature (SHA256SUMS.sig) over both .deb files, verifiable with the project's published release-signing public key.")

if [ "$channel" = "beta" ]; then
  gh_args+=(--prerelease)
fi

gh "${gh_args[@]}"
