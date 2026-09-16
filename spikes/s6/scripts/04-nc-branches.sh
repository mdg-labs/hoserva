#!/usr/bin/env bash
# Step 4 (NC) — uses the already-mounted ctm2 share (02-topology-up.sh:
# cache/ctm2=RW : disk1/ctm2=NC : disk2/ctm2=NC : disk3/ctm2=NC) to confirm
# man/mergerfs.1's NC description exactly: "NC: (no-create) - Will be
# excluded from create policies. You can't create on that branch but you
# can change or delete."
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

OUT=${S6_OUT:-$LAB/s6-results}
mkdir -p -- "$OUT"

mountpoint -q -- "$LAB/mnt/user/ctm2" || die "ctm2 is not mounted — run 02-topology-up.sh first"

s6_log "== a file already on an NC (array) branch stays readable through the share =="
dd if=/dev/urandom of="$LAB/mnt/disk2/ctm2/existing.bin" bs=1M count=3 status=none
sha_direct=$(sha256sum "$LAB/mnt/disk2/ctm2/existing.bin" | awk '{print $1}')
sha_via_share=$(sha256sum "$LAB/mnt/user/ctm2/existing.bin" | awk '{print $1}')
[[ "$sha_direct" == "$sha_via_share" ]] || die "existing.bin, written directly to disk2 (an NC branch), reads back with a different sha256 through the share (direct=$sha_direct via-share=$sha_via_share)"
s6_log "confirmed: existing.bin (written directly to disk2, an NC branch) reads back byte-identical through the ctm2 share (sha256 $sha_via_share)"

s6_log "== new files written through the share never land on an NC branch =="
for i in 1 2 3 4 5; do
  dd if=/dev/urandom of="$LAB/mnt/user/ctm2/new-$i.bin" bs=256K count=1 status=none
done
{
  for i in 1 2 3 4 5; do
    s6_locate "new-$i.bin" "$LAB/mnt/cache/ctm2" "$LAB/mnt/disk1/ctm2" "$LAB/mnt/disk2/ctm2" "$LAB/mnt/disk3/ctm2"
  done
} | tee "$OUT/nc-new-file-locations.log"
for i in 1 2 3 4 5; do
  [[ -e "$LAB/mnt/cache/ctm2/new-$i.bin" ]] || die "new-$i.bin did not land on cache"
  [[ -e "$LAB/mnt/disk1/ctm2/new-$i.bin" || -e "$LAB/mnt/disk2/ctm2/new-$i.bin" || -e "$LAB/mnt/disk3/ctm2/new-$i.bin" ]] && die "new-$i.bin landed on an NC (array) branch — NC did not exclude it from create"
done
s6_log "confirmed: all 5 new files landed on cache only; none landed on disk1/disk2/disk3 (the NC branches)"

s6_log "== NC still permits change and delete (man/mergerfs.1: 'you can change or delete') =="
echo -n "changed" >> "$LAB/mnt/user/ctm2/existing.bin"
sha_after_change=$(sha256sum "$LAB/mnt/disk2/ctm2/existing.bin" | awk '{print $1}')
[[ "$sha_after_change" != "$sha_direct" ]] || die "appending through the share did not change existing.bin's content on disk2 — the change did not take effect"
s6_log "confirmed: appending to existing.bin through the share changed its content on disk2 directly (sha256 now $sha_after_change) — NC permits change, as documented"
rm -f -- "$LAB/mnt/user/ctm2/existing.bin"
[[ -e "$LAB/mnt/disk2/ctm2/existing.bin" ]] && die "existing.bin still present on disk2 after deleting it through the share — NC should still permit delete"
s6_log "confirmed: deleting existing.bin through the share removed it from disk2 — NC permits delete, as documented"
