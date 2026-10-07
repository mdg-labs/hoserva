#!/usr/bin/env bash
# Tests scripts/release/collect-build-output.sh and
# scripts/release/stage-release-artifacts.sh: a collected artifact stages
# cleanly, and a download that is empty, partial, padded, altered or not made
# of regular files is refused. The .deb files are placeholders.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
collect="$script_dir/collect-build-output.sh"
stage="$script_dir/stage-release-artifacts.sh"

fail=0
note() { printf 'test-stage-release-artifacts: %s\n' "$*" >&2; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

tag=v1.2.3
amd64=hoserva_1.2.3_amd64.deb
arm64=hoserva_1.2.3_arm64.deb

fresh() {
  rm -rf "$work/dist" "$work/artifact" "$work/staged"
  mkdir -p "$work/dist"
  printf 'amd64 deb\n' >"$work/dist/$amd64"
  printf 'arm64 deb\n' >"$work/dist/$arm64"
  printf 'unrelated\n' >"$work/dist/other.txt"
  "$collect" "$tag" "$work/dist" "$work/artifact"
}

expect_refused() {
  local label="$1"
  if "$stage" "$tag" "$work/artifact" "$work/staged" >/dev/null 2>&1; then
    note "FAIL: $label was accepted"
    fail=1
  fi
}

fresh
if ! "$stage" "$tag" "$work/artifact" "$work/staged"; then
  note "FAIL: a collected artifact was refused"
  fail=1
fi
if [ "$(find "$work/artifact" -mindepth 1 -printf '%f\n' | LC_ALL=C sort | tr '\n' ' ')" != "hoserva-debs.sha256 $amd64 $arm64 " ]; then
  note "FAIL: the collected artifact does not hold exactly the two packages and their sums"
  fail=1
fi
if [ "$(find "$work/staged" -mindepth 1 -printf '%f\n' | LC_ALL=C sort | tr '\n' ' ')" != "$amd64 $arm64 " ]; then
  note "FAIL: staging did not produce exactly the two packages"
  fail=1
fi
cmp -s "$work/dist/$amd64" "$work/staged/$amd64" || { note "FAIL: staged amd64 package differs"; fail=1; }

fresh
rm "$work/artifact/$arm64"
expect_refused "an artifact missing a package"

fresh
rm -f "$work/artifact"/*
expect_refused "an empty artifact"

fresh
rm -rf "$work/artifact"
expect_refused "a missing download directory"

fresh
printf 'x\n' >"$work/artifact/extra.txt"
expect_refused "an artifact with an extra file"

fresh
mkdir "$work/artifact/sub"
expect_refused "an artifact with a subdirectory"

fresh
printf 'tampered\n' >"$work/artifact/$amd64"
expect_refused "a package that does not match its sum"

fresh
grep -F "$amd64" "$work/artifact/hoserva-debs.sha256" >"$work/one"
mv "$work/one" "$work/artifact/hoserva-debs.sha256"
expect_refused "sums that list one package"

fresh
rm "$work/artifact/hoserva-debs.sha256"
expect_refused "an artifact without sums"

fresh
: >"$work/artifact/$arm64"
(cd "$work/artifact" && sha256sum "$amd64" "$arm64" >hoserva-debs.sha256)
expect_refused "an empty package with a matching sum"

fresh
mv "$work/artifact/$arm64" "$work/real"
ln -s "$work/real" "$work/artifact/$arm64"
expect_refused "a symlinked package"

fresh
mkdir -p "$work/staged"
printf 'x\n' >"$work/staged/leftover"
expect_refused "a non-empty staging directory"

fresh
if "$stage" v1.2 "$work/artifact" "$work/staged" >/dev/null 2>&1; then
  note "FAIL: a malformed tag was accepted"
  fail=1
fi

fresh
rm "$work/dist/$arm64"
rm -rf "$work/artifact2"
if "$collect" "$tag" "$work/dist" "$work/artifact2" >/dev/null 2>&1; then
  note "FAIL: collecting with a missing package succeeded"
  fail=1
fi

[ "$fail" -eq 0 ] || exit 1
note "ok"
