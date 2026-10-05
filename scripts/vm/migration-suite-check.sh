#!/usr/bin/env bash
# Self-check of the migration suite's own assertions (doc 06 §5, issue #80): no
# VM, no libvirt and no real HOSERVA_LAB_ID. It sources run-migration-suite.sh
# and runs the functions that decide whether a scan matches a fixture against
# scan reports captured from `hoserva --json migrate status` in an L3 guest
# (scripts/vm/testdata/migration-suite/), and each time shows that a changed
# report, a changed manifest or a different refusal fails the assertion, so a
# green suite is not a suite that cannot fail. It also runs the guest helper
# against a scratch tree.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
data="$script_dir/testdata/migration-suite"
fail=0
ok() { echo "ok: $*"; }
bad() {
  echo "FAIL: $*"
  fail=1
}

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT

# the reports carry the serials of lab 80-a1
export HOSERVA_LAB_ID=80-a1
# shellcheck source=scripts/vm/run-migration-suite.sh
source "$script_dir/run-migration-suite.sh"
VM_REPO_ROOT="$repo_root"
VM_ROOT="$work/vm"
LAYOUT=separate

# use_variant VARIANT SCAN-JSON: the fixture's expected files of this variant and
# the report to check, as the suite has them in a lab
use_variant() {
  VARIANT=$1
  mkdir -p -- "$VM_ROOT/unraid/$VARIANT/expected"
  # a refusal variant is not compared with a manifest
  : >"$VM_ROOT/unraid/$VARIANT/expected/manifest.sha256"
  : >"$VM_ROOT/unraid/$VARIANT/expected/entries.tsv"
  [[ ! -f "$data/$VARIANT.manifest.sha256" ]] || cp -- "$data/$VARIANT.manifest.sha256" "$VM_ROOT/unraid/$VARIANT/expected/manifest.sha256"
  [[ ! -f "$data/$VARIANT.entries.tsv" ]] || cp -- "$data/$VARIANT.entries.tsv" "$VM_ROOT/unraid/$VARIANT/expected/entries.tsv"
  load_variant
  SCAN_JSON=$(cat -- "$data/$VARIANT.scan.json")
}

# scan_fails DESCRIPTION JQ-FILTER SUBSTRING: assert_scan, run on the report the
# filter changes, fails and says SUBSTRING
scan_fails() {
  local desc=$1 filter=$2 want=$3 saved=$SCAN_JSON
  SCAN_JSON=$(jq "$filter" <<<"$saved")
  if assert_scan; then
    bad "$desc was accepted"
  elif [[ "$ASSERT_OUT" != *"$want"* ]]; then
    bad "$desc failed for another reason: $ASSERT_OUT"
  else
    ok "$desc fails the assertion"
  fi
  SCAN_JSON=$saved
}

# --- a supported variant
v7=unraid-7x-xfs-single-parity
use_variant "$v7"
if assert_scan; then
  ok "the 7.x report matches its fixture"
else
  bad "the 7.x report does not match its fixture: $ASSERT_OUT"
fi
expect_eq() {  # description, got, want
  if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1: got '$2', want '$3'"; fi
}
expect_eq "a hidden top-level directory is not counted on disk2" "$(expected_baseline slot disk2)" "9 0 0"
expect_eq "disk1 holds 14 files, 3 symlinks and 3 special files" "$(expected_baseline slot disk1)" "14 3 3"
expect_eq "appdata on the data disks holds 5 files, 3 symlinks and 3 special files (the cache's copy is not counted)" "$(expected_baseline share appdata)" "5 3 3"

scan_fails "a file more on disk1" '(.report.rows[] | select(.check == "baseline" and .subject == "disk1") | .detail) |= sub("^14 files"; "15 files")' "disk1: the baseline counts 15 files"
scan_fails "a symlink fewer on disk1" '(.report.rows[] | select(.check == "baseline" and .subject == "disk1") | .detail) |= sub("3 symlinks"; "2 symlinks")' "disk1: the baseline counts"
scan_fails "a share's file count changed" '(.report.rows[] | select(.check == "baseline" and .subject == "media") | .detail) |= sub("^10 files"; "9 files")' "share media"
scan_fails "a missing baseline row" '.report.rows |= map(select(.subject != "disk3" or .check != "baseline"))' "disk3: the report has no baseline row"
scan_fails "a refused disk the fixture adopts" '(.report.review.disks[] | select(.slot == "disk2")) |= (.refused = true)' "disk2: refused is true"
scan_fails "another serial for disk1" '(.report.review.disks[] | select(.slot == "disk1") | .serial) = "other"' "disk1: the report matched serial"
scan_fails "another filesystem for disk3" '(.report.review.disks[] | select(.slot == "disk3") | .filesystem) = "ext4"' "disk3: the report says filesystem"
scan_fails "no proposed role for disk1" 'del(.report.review.disks[] | select(.slot == "disk1") | .proposedRole)' "disk1: the proposed role"
scan_fails "a no-go verdict" '.report.verdict = "no_go"' "a supported variant must not be refused"
scan_fails "a refuse row" '.report.rows += [{"check": "data_disks", "status": "refuse", "subject": "disk1", "detail": "x"}]' "the report has refuse rows"
scan_fails "a scan that did not finish" '.phase = "scan_failed"' "not scanned"

# a manifest with one more file on disk1 is a different expected result
printf '%s\t%s\t%s\t%s\t%s\t%s\n' 0000000000000000000000000000000000000000000000000000000000000000 1 666 99:100 disk1 documents/extra.txt >>"$VM_ROOT/unraid/$v7/expected/manifest.sha256"
if assert_scan; then bad "a manifest with another file was accepted"; elif [[ "$ASSERT_OUT" == *"disk1: the baseline counts 14 files"* ]]; then ok "a manifest with another file fails the assertion"; else bad "another manifest failed for another reason: $ASSERT_OUT"; fi

# the file step 9 deletes is plain, not appdata and on an array-only share
pick=$(pick_fix_file "backup,media")
IFS=$'\t' read -r _ disk path <<<"$pick"
if [[ -n "$pick" && "$path" != appdata/* && "$path" =~ ^(backup|media)/ && "$disk" != cache ]]; then ok "step 9 picks $path on $disk"; else bad "step 9 picked '$pick'"; fi
expect_eq "step 9 picks nothing when no share is array-only" "$(pick_fix_file "nothing")" ""

# --- a refusal variant
for v in unraid-encrypted unraid-zfs-disk unraid-corrupt-xfs; do
  use_variant "$v"
  if [[ "$v" == unraid-zfs-disk ]]; then
    # the two devices of one btrfs filesystem are refused as multi-device
    # members, with the message datadisks.go words for it (Q23)
    SCAN_JSON=$(jq '
      (.report.review.disks[] | select(.slot == "disk4" or .slot == "disk5")) |= (.refusalCode = "multi_device_btrfs")
      | (.report.rows[] | select(.status == "refuse" and (.subject == "disk4" or .subject == "disk5"))) |= (.detail = (.subject + " is not adopted: it is one of 2 devices of a btrfs filesystem, which is not a self-contained filesystem per disk (Q23)"))' <<<"$SCAN_JSON")
  fi
  if assert_scan; then ok "the $v report matches its fixture"; else bad "the $v report does not match its fixture: $ASSERT_OUT"; fi
  scan_fails "$v: a verdict that is not no_go" '.report.verdict = "go_with_warnings"' "not no_go"
  case "$v" in
    unraid-encrypted) slot=disk2 ;;
    unraid-zfs-disk) slot=disk3 ;;
    unraid-corrupt-xfs) slot=disk2 ;;
  esac
  scan_fails "$v: a refused disk that is not refused" "(.report.review.disks[] | select(.slot == \"$slot\")) |= (.refused = false)" "$slot: refused is false"
  scan_fails "$v: another refusal code" "(.report.review.disks[] | select(.slot == \"$slot\") | .refusalCode) = \"unreadable\"" "$slot: refusal code"
  scan_fails "$v: another refusal message" "(.report.rows[] | select(.status == \"refuse\" and .subject == \"$slot\") | .detail) |= sub(\"not adopted\"; \"not taken\")" "$slot: the refusal message"
  scan_fails "$v: a parity disk that is refused" '(.report.review.disks[] | select(.slot == "parity")) |= (.refused = true)' "parity: refused is true"
done

# --- a fixture whose disks are of three filesystem types, scanned in full
v=unraid-btrfs-and-ext4-disks
use_variant "$v"
if assert_scan; then
  ok "the $v report matches its fixture"
else
  bad "the $v report does not match its fixture: $ASSERT_OUT"
fi
scan_fails "$v: a baseline row of disk4 missing" '.report.rows |= map(select(.subject != "disk4" or .check != "baseline"))' "disk4: the report has no baseline row"
scan_fails "$v: another filesystem for the ext4 disk" '(.report.review.disks[] | select(.slot == "disk4") | .filesystem) = "xfs"' "disk4: the report says filesystem"
scan_fails "$v: another filesystem for the btrfs disk" '(.report.review.disks[] | select(.slot == "disk3") | .filesystem) = "xfs"' "disk3: the report says filesystem"
scan_fails "$v: no proposed role for the btrfs disk" 'del(.report.review.disks[] | select(.slot == "disk3") | .proposedRole)' "disk3: the proposed role"
scan_fails "$v: another size for the first data disk" '(.report.review.disks[] | select(.slot == "disk1") | .size) = 335544320' "disk1: the report says size"
scan_fails "$v: another size for the cache" '(.report.review.disks[] | select(.slot == "pool cache") | .size) = 1073741824' "cache: the report says size"

# --- a spec that says capture=none: the scan names no data disk and records no
# baseline. No committed fixture is built so; the report is one captured from
# unraid-btrfs-and-ext4-disks without its config/hoserva/, before its guest disks
# grew, so this spec has the sizes of that run's disks.
real_root=$VM_REPO_ROOT
VM_REPO_ROOT="$work/repo"
v=unraid-no-capture
mkdir -p -- "$VM_REPO_ROOT/testdata/unraid-fixtures/$v"
cat >"$VM_REPO_ROOT/testdata/unraid-fixtures/$v/spec" <<'EOF'
capture=none
disk parity kind=parity fs=none  size=384M target=parity1
disk disk1  kind=data   fs=xfs   size=320M target=disk1
disk disk2  kind=data   fs=xfs   size=320M target=disk2
disk disk3  kind=data   fs=btrfs size=320M target=disk3
disk disk4  kind=data   fs=ext4  size=320M target=disk4
disk cache  kind=pool   fs=btrfs size=64G  target=cache pool=cache
EOF
printf 'disk parity parity\ndisk disk1 adopt\ndisk disk2 adopt\ndisk disk3 adopt\ndisk disk4 adopt\ndisk cache recreated\nwarn capture-missing\n' >"$VM_REPO_ROOT/testdata/unraid-fixtures/$v/expect"
use_variant "$v"
if assert_scan; then ok "the $v report matches its fixture"; else bad "the $v report does not match its fixture: $ASSERT_OUT"; fi
scan_fails "$v: a baseline row" '.report.rows += [{"check": "baseline", "status": "info", "subject": "disk1", "detail": "1 file (1 B), 0 symlinks and 0 special files; 1 file (1 B) hashed."}]' "has baseline rows"
scan_fails "$v: a disk of the fixture missing from the table" 'del(.report.review.disks[] | select(.serial == "disk3-hoserva-80-a1"))' "no row for disk3"
scan_fails "$v: another filesystem for the btrfs disk" '(.report.review.disks[] | select(.serial == "disk3-hoserva-80-a1") | .filesystem) = "xfs"' "disk3: the report says filesystem"
scan_fails "$v: a capture that is not missing" '.report.review.capture.state = "present"' "the capture state is"
scan_fails "$v: another size for the cache" '(.report.review.disks[] | select(.serial == "cache-hoserva-80-a1") | .size) = 60129542144' "cache: the report says size"
VM_REPO_ROOT=$real_root

# --- the guest helper on a scratch tree
exp="$work/expected"
root="$work/root"
mkdir -p "$exp" "$root/documents" "$root/media"
printf 'alpha' >"$root/documents/a.txt"
printf 'beta' >"$root/media/b.bin"
sha_a=$(sha256sum "$root/documents/a.txt" | cut -d' ' -f1)
sha_b=$(sha256sum "$root/media/b.bin" | cut -d' ' -f1)
{
  printf '# manifest\n'
  printf '%s\t5\t666\t99:100\tdisk1\tdocuments/a.txt\n' "$sha_a"
  printf '%s\t4\t666\t99:100\tdisk2\tmedia/b.bin\n' "$sha_b"
  printf '%s\t4\t666\t99:100\tcache\tmedia/only-on-cache.bin\n' "$sha_b"
} >"$exp/manifest.sha256"
helper="$script_dir/migration-suite-guest.sh"
if out=$("$helper" files "$exp" "$root" cache 2>&1); then ok "the guest helper accepts files with the manifest's sha256: $out"; else bad "the guest helper refused a correct tree: $out"; fi
printf 'gamma' >"$root/media/b.bin"
if out=$("$helper" files "$exp" "$root" cache 2>&1); then bad "the guest helper accepted a changed file"; elif [[ "$out" == *"another sha256"*"media/b.bin"* ]]; then ok "the guest helper names a file with another sha256"; else bad "changed file: $out"; fi
printf 'beta' >"$root/media/b.bin"
rm "$root/documents/a.txt"
if out=$("$helper" files "$exp" "$root" cache 2>&1); then bad "the guest helper accepted a missing file"; elif [[ "$out" == *"files missing"*"documents/a.txt"* ]]; then ok "the guest helper names a missing file"; else bad "missing file: $out"; fi
printf 'alpha' >"$root/documents/a.txt"
if out=$("$helper" files "$exp" "$root" "" 2>&1); then bad "the guest helper accepted a tree without the cache's file"; elif [[ "$out" == *"only-on-cache.bin"* ]]; then ok "the guest helper only skips the slots it is told are pools"; else bad "pool rows: $out"; fi

exit "$fail"
