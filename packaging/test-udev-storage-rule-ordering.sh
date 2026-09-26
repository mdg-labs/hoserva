#!/usr/bin/env bash
# #372: udev evaluates same-priority rule *files* in filename order, and
# Debian's own udev package ships /lib/udev/rules.d/60-persistent-
# storage.rules, whose IMPORT{builtin}="blkid" is what actually sets
# ENV{ID_FS_UUID} for a block device — the property packaging/debian/
# hoserva-storage.rules's own condition tests. A first version of this
# rule installed as "60-hoserva-storage.rules" sorted alphabetically
# *before* that file ("h" < "p") and so never matched at all — confirmed
# directly with `udevadm test` inside the L3 lab; the nightly L3
# workflow's own live disk-return recovery failed for exactly this
# reason. debian/rules must install this file under a name that sorts
# after "60-persistent-storage.rules", and never rely on dh_installudev's
# own debian/hoserva.udev naming convention to get that right by chance.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
rules="$script_dir/debian/rules"
source_file="$script_dir/debian/hoserva-storage.rules"

fail=0
note() { printf 'test-udev-storage-rule-ordering: %s\n' "$*" >&2; }

if [ ! -f "$source_file" ]; then
  note "FAIL: missing $source_file"
  exit 1
fi
if [ -f "$script_dir/debian/hoserva.udev" ]; then
  note "FAIL: debian/hoserva.udev exists — dh_installudev's own default naming convention would install this file a second time, under a name this repo does not control"
  fail=1
fi

install_line="$(grep -E 'lib/udev/rules\.d/' "$rules" || true)"
if [ -z "$install_line" ]; then
  note "FAIL: debian/rules has no explicit install line for a /lib/udev/rules.d/ destination"
  exit 1
fi

installed_name="$(grep -oE '[^/[:space:]]+\.rules$' <<<"$install_line" || true)"
if [ -z "$installed_name" ]; then
  note "FAIL: could not parse the installed rule file's own name from: $install_line"
  exit 1
fi

# Sorted lexically alongside the exact Debian-shipped filename this rule
# depends on: the installed name must sort strictly after it.
sorted_first="$(printf '%s\n%s\n' "$installed_name" "60-persistent-storage.rules" | sort | head -n1)"
if [ "$sorted_first" != "60-persistent-storage.rules" ]; then
  note "FAIL: installed rule file '$installed_name' does not sort after 60-persistent-storage.rules — ENV{ID_FS_UUID} would still be empty when it runs"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS ($installed_name)"
fi
exit "$fail"
