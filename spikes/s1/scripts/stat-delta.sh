#!/usr/bin/env bash
# Computes and prints named, per-field deltas between two snapshot.sh lines
# for the same disk (spike S1, doc 06 §6). A previous attempt's write-up
# misattributed a delta to fields it never checked because it parsed the
# raw snapshot line by eye against a comment that listed only 11 of the
# kernel's 17 stat fields — every delta from here on is computed by this
# script and named, never parsed by eye.
#
# Usage: stat-delta.sh "<before line>" "<after line>"
# Each argument is one line of snapshot.sh's own output for one disk:
#   "<img> <dev> <17 space-separated stat fields>"
#
# Field order (kernel Documentation/admin-guide/iostats.rst, confirmed by
# counting the fields this kernel actually prints):
#  1 read_ios        2 read_merges     3 read_sectors    4 read_ticks
#  5 write_ios       6 write_merges    7 write_sectors   8 write_ticks
#  9 in_flight       10 io_ticks       11 time_in_queue
# 12 discard_ios     13 discard_merges 14 discard_sectors 15 discard_ticks
# 16 flush_ios       17 flush_ticks
set -euo pipefail

names=(read_ios read_merges read_sectors read_ticks write_ios write_merges write_sectors write_ticks in_flight io_ticks time_in_queue discard_ios discard_merges discard_sectors discard_ticks flush_ios flush_ticks)

before_line=${1:?usage: stat-delta.sh "<before line>" "<after line>"}
after_line=${2:?usage: stat-delta.sh "<before line>" "<after line>"}
read -r -a before <<< "$before_line"
read -r -a after <<< "$after_line"

img=${before[0]:-?}
if [ "${#before[@]}" -lt 19 ] || [ "${#after[@]}" -lt 19 ]; then
  echo "$img: (short snapshot line, expected <img> <dev> + 17 fields — got ${#before[@]}/${#after[@]} tokens)" >&2
  exit 1
fi

changed=0
for i in "${!names[@]}"; do
  idx=$((i + 2))
  b=${before[$idx]:-0}
  a=${after[$idx]:-0}
  if [ "$b" != "$a" ]; then
    changed=1
    echo "$img ${names[$i]}: $b -> $a (delta $((a - b)))"
  fi
done
if [ "$changed" -eq 0 ]; then
  echo "$img: (no field changed)"
fi
