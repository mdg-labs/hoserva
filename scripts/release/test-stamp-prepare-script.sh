#!/usr/bin/env bash
# Tests for scripts/release/stamp-prepare-script.sh: the released copy of
# the Unraid prepare script carries the tag as its version, differs from the
# source in that one line only, is runnable, and its .sha256 verifies with
# `sha256sum -c` exactly as doc 05 §4 tells the user to run it.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
stamp="$script_dir/stamp-prepare-script.sh"
src="$repo_root/tools/unraid/prepare-migration.sh"

fail=0
note() { printf 'test-stamp-prepare-script: %s\n' "$*" >&2; }
assert_eq() {
  if [ "$1" != "$2" ]; then
    note "FAIL: expected '$2', got '$1' ($3)"
    fail=1
  fi
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

"$stamp" v1.2.3 "$work/out"
assert_eq "$(find "$work/out" -type f -printf '%f\n' | LC_ALL=C sort | tr '\n' ' ')" "prepare-migration.sh prepare-migration.sh.sha256 " "the two release assets"
assert_eq "$("$work/out/prepare-migration.sh" --version)" "v1.2.3" "the stamped script reports the tag"
assert_eq "$("$src" --version)" "unreleased" "the source stays unstamped"
assert_eq "$(diff "$src" "$work/out/prepare-migration.sh" | grep -c '^[<>]')" 2 "only the version line differs"
if (cd "$work/out" && sha256sum -c prepare-migration.sh.sha256 >/dev/null 2>&1); then :; else
  note "FAIL: sha256sum -c rejects the released checksum file"
  fail=1
fi
echo tampered >>"$work/out/prepare-migration.sh"
if (cd "$work/out" && sha256sum -c prepare-migration.sh.sha256 >/dev/null 2>&1); then
  note "FAIL: sha256sum -c accepts a modified script"
  fail=1
fi

"$stamp" v1.2.3-beta.4 "$work/beta"
assert_eq "$("$work/beta/prepare-migration.sh" --version)" "v1.2.3-beta.4" "beta tag"

for bad in 1.2.3 'v1.2.3;rm' 'v1.2' 'v1.2.3-rc.1' ''; do
  if "$stamp" "$bad" "$work/bad" >/dev/null 2>&1; then
    note "FAIL: tag '$bad' was accepted"
    fail=1
  fi
done
[ ! -e "$work/bad" ] || {
  note "FAIL: a refused tag still wrote output"
  fail=1
}

printf '#!/usr/bin/env bash\necho hi\n' >"$work/noversion.sh"
if HOSERVA_PREPARE_SCRIPT="$work/noversion.sh" "$stamp" v1.2.3 "$work/nv" >/dev/null 2>&1; then
  note "FAIL: a script without the version line was stamped"
  fail=1
fi

[ "$fail" -eq 0 ] || exit 1
note "ok"
