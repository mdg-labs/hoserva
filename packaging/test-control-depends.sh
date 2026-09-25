#!/usr/bin/env bash
# #331: the design (D9, doc 14 §7, doc 03 §1 Step 2) assumes the one
# `.deb` install always has Samba and NFS available — share creation
# generates smb.conf/exports for both, and doc 03 §1 Step 2's system
# check says a missing storage dependency "should be impossible, they
# are .deb dependencies". packaging/debian/control's Depends: must
# therefore list samba and nfs-kernel-server as hard dependencies, not
# Recommends, the same way it already does for mergerfs/snapraid.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
control="$script_dir/debian/control"

fail=0
note() { printf 'test-control-depends: %s\n' "$*" >&2; }

if [ ! -f "$control" ]; then
  note "FAIL: missing $control"
  exit 1
fi

depends_line="$(grep -E '^Depends:' "$control" || true)"

if [ -z "$depends_line" ]; then
  note "FAIL: packaging/debian/control has no Depends: line"
  exit 1
fi

# Whole-token match on the comma-separated Depends list: strips the
# "Depends:" prefix and any version constraint, so "samba" doesn't
# false-match a hypothetical "libsamba-foo" and "nfs-kernel-server"
# isn't satisfied by some other nfs-* package.
mapfile -t depends_tokens < <(
  sed -E 's/^Depends:[[:space:]]*//' <<<"$depends_line" \
    | tr ',' '\n' \
    | sed -E 's/^[[:space:]]+|[[:space:]]+$//g' \
    | sed -E 's/[[:space:]]*\([^)]*\)$//'
)

has_token() {
  local want="$1" tok
  for tok in "${depends_tokens[@]}"; do
    [ "$tok" = "$want" ] && return 0
  done
  return 1
}

if ! has_token "samba"; then
  note "FAIL: packaging/debian/control Depends: does not include samba (D9, doc 14 §7, doc 03 §1 Step 2)"
  fail=1
fi

if ! has_token "nfs-kernel-server"; then
  note "FAIL: packaging/debian/control Depends: does not include nfs-kernel-server (D9, doc 14 §7, doc 03 §1 Step 2)"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
