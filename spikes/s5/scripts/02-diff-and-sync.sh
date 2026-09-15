#!/usr/bin/env bash
# Step 2: exercise every category "diff" reports (snapraid.txt §5.5 — equal,
# added, removed, updated, moved, copied) with one change each, on the array
# 01-seed-and-sync.sh already synced, then sync again to commit them.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

[[ -f "$CONF" ]] || die "run 01-seed-and-sync.sh first"

echo "### add: a new file on disk1 ###"
head -c 500000 /dev/urandom > "$LAB/mnt/disk1/photos/img0002.bin"

echo "### updated: rewrite an existing file's content (same path) ###"
head -c 400000 /dev/urandom > "$LAB/mnt/disk2/documents/manual.bin"

echo "### removed: delete an existing file ###"
rm -f "$LAB/mnt/disk3/isos/debian-netinst.bin"

echo "### moved: same disk, different directory, same name/size/mtime (mv preserves inode+mtime) ###"
mkdir -p "$LAB/mnt/disk1/movies/archive"
mv "$LAB/mnt/disk1/movies/gattaca.bin" "$LAB/mnt/disk1/movies/archive/gattaca.bin"

echo "### copied: same file content copied to a new path, same disk, mtime preserved (cp -p) ###"
cp -p "$LAB/mnt/disk2/movies/moon.bin" "$LAB/mnt/disk2/movies/moon-copy.bin"

sync

echo "### diff after all five changes ###"
s5_snapraid_log "$S5/diff-exercises.log" diff
echo "[diff exit $S5_LAST_RC]"
[[ "$S5_LAST_RC" -eq 2 ]] || die "expected diff exit 2 (changes present), got $S5_LAST_RC"

grep -qF "add photos/img0002.bin" "$S5/diff-exercises.log" || die "expected diff line matching 'add photos/img0002.bin'"
grep -qF "remove isos/debian-netinst.bin" "$S5/diff-exercises.log" || die "expected diff line matching 'remove isos/debian-netinst.bin'"

echo "### sync to commit the changes ###"
s5_snapraid sync

echo "### post-sync diff (expect: everything equal again) ###"
s5_snapraid diff

s5_array_manifest disk1 disk2 disk3 > "$S5/manifest-after-diff-exercises.sha256"
echo "manifest: $S5/manifest-after-diff-exercises.sha256 ($(wc -l < "$S5/manifest-after-diff-exercises.sha256") files)"
