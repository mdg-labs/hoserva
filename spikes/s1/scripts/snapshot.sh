#!/usr/bin/env bash
# Prints per-disk /sys/block/<dev>/stat counters for the lab's loop disks
# (spike S1, doc 06 §6). Run only inside the lab container, piped in via
# `docker compose exec -T lab bash -s -- < snapshot.sh` — never on the host.
set -euo pipefail

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"
LAB="/lab/$HOSERVA_LAB_ID"

echo "# snapshot $(date -u +%FT%TZ)"
for img in parity1 disk1 disk2 disk3 cache; do
  dev=$(losetup -j "$LAB/img/$img.img" --output NAME --noheadings 2>/dev/null | tr -d '[:space:]')
  if [ -z "$dev" ]; then
    echo "$img MISSING"
    continue
  fi
  base=$(basename "$dev")
  # /sys/block/<dev>/stat fields (17 on this kernel, confirmed by counting
  # the line itself — kernel Documentation/admin-guide/iostats.rst; a
  # previous attempt's comment here only listed 11 and a delta was
  # misattributed to the missing ones as a result, spike S1 attempt 3):
  #  1 read_ios       2 read_merges    3 read_sectors   4 read_ticks
  #  5 write_ios      6 write_merges   7 write_sectors  8 write_ticks
  #  9 in_flight      10 io_ticks      11 time_in_queue
  # 12 discard_ios    13 discard_merges 14 discard_sectors 15 discard_ticks
  # 16 flush_ios      17 flush_ticks
  # Never parse this by eye — use scripts/stat-delta.sh or
  # scripts/window-delta.sh, which read this same field list by name.
  echo "$img $dev $(cat "/sys/block/$base/stat")"
done
