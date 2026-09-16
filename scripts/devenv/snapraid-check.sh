#!/usr/bin/env bash
# Run one SnapRAID sync against this lab's standing array and confirm it
# succeeded (issue #121).
#
# doc 06 §3 describes L2 as the layer that covers the SnapRAID operations
# doc 02 §2 drives, but nothing in the harness actually ran one: CI's lab job
# built the array and tore it down again, so a broken snapraid package or a
# mis-generated config would not have been noticed here.
#
# This is deliberately the smallest real check — write a config, sync, and
# require SnapRAID's own success line. Full operation coverage (diff, scrub,
# fix, reconstruction) lives in spikes/s5, not in the harness.
#
# Run only inside the lab container, via `make lab-snapraid-check`.
set -euo pipefail
HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source "$HERE/lib.sh"
lab_require_id

command -v snapraid >/dev/null 2>&1 \
  || die "snapraid is not installed in this image — it is expected to be (scripts/devenv/Dockerfile)"

[[ -d "$LAB/mnt/parity1" ]] || die "no parity disk at \$LAB/mnt/parity1 — run create-array.sh first"

conf="$LAB/snapraid.conf"
{
  echo "parity $LAB/mnt/parity1/snapraid.parity"
  echo "content $LAB/mnt/parity1/snapraid.content"
  for d in "$LAB"/mnt/disk*/; do
    [[ -d "$d" ]] || continue
    name=$(basename "${d%/}")
    echo "content $d.snapraid.content"
    echo "data $name $d"
  done
  # Exclude the content files themselves and anything the pool mount exposes:
  # /mnt/user is a mergerfs view of the same data disks, so including it would
  # make SnapRAID see every file twice.
  echo "exclude *.content*"
  echo "exclude /mnt/user/"
} > "$conf"

echo "## snapraid $(dpkg-query -W -f '${Version}' snapraid 2>/dev/null || echo 'version unknown')"
echo "## config: $conf"
cat "$conf"

log="$LAB/snapraid-sync.log"
echo "## snapraid sync"
if ! snapraid -c "$conf" sync 2>&1 | tee "$log"; then
  die "snapraid sync failed — see $log"
fi

# SnapRAID exits 0 on some non-fatal paths, so require its own completion
# line rather than trusting the status alone.
grep -q 'Everything OK' "$log" \
  || die "snapraid sync did not report 'Everything OK' — see $log"

echo "snapraid sync: confirmed (exit 0, 'Everything OK')"
