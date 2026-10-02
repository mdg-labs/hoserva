#!/usr/bin/env bash
# Tests that scripts/release/publish-release.sh attaches the Unraid prepare
# script and its checksum to the release (doc 05 §4 Phase A step 0), next to
# the .deb files. `gh` is a stand-in that records its arguments; the .deb
# files are placeholders and the signing key is a throwaway.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

fail=0
note() { printf 'test-publish-release: %s\n' "$*" >&2; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/bin" "$work/dist"
cat >"$work/bin/gh" <<'GH'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$GH_ARGS_FILE"
GH
chmod +x "$work/bin/gh"

printf 'deb\n' >"$work/dist/hoserva_1.2.3_amd64.deb"
printf 'deb\n' >"$work/dist/hoserva_1.2.3_arm64.deb"
openssl genpkey -algorithm ed25519 -out "$work/key.pem" >/dev/null 2>&1

PATH="$work/bin:$PATH" GH_ARGS_FILE="$work/gh-args" GITHUB_REPOSITORY=example/hoserva \
  "$script_dir/publish-release.sh" v1.2.3 "$work/dist" "$work/key.pem"

for asset in prepare-migration.sh prepare-migration.sh.sha256; do
  if grep -qx -- "$work/dist/$asset" "$work/gh-args"; then :; else
    note "FAIL: the release does not attach $asset"
    fail=1
  fi
done
if [ "$("$work/dist/prepare-migration.sh" --version)" != v1.2.3 ]; then
  note "FAIL: the attached prepare script is not stamped with the tag"
  fail=1
fi
if ! (cd "$work/dist" && sha256sum -c prepare-migration.sh.sha256 >/dev/null 2>&1); then
  note "FAIL: the attached checksum does not verify the attached script"
  fail=1
fi
if ! grep -qx -- "$work/dist/SHA256SUMS.sig" "$work/gh-args"; then
  note "FAIL: the .deb signature is no longer attached"
  fail=1
fi

[ "$fail" -eq 0 ] || exit 1
note "ok"
