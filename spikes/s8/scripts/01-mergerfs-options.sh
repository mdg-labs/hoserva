#!/usr/bin/env bash
# S8 step 1: mount doc 02 §1's full mergerfs option table on the standing
# lab array's disk1..3 branches (a second mount, alongside make lab-up's
# own `user` pool, exactly as S6's fallback test ran a second pool over
# the same disks) and confirm every option's *effective* value as the
# running mount reports it via the runtime control file
# (man/mergerfs.1 "RUNTIME": getfattr -d <mountpoint>/.mergerfs).
set -euo pipefail
LAB=/lab/9-a1
OUT=$LAB/s8
mkdir -p "$OUT"
MNT=$LAB/mnt/pool-s8
mkdir -p "$MNT"

echo "== mergerfs pkg version (metadata, never --version) ==" | tee "$OUT/mergerfs-options.log"
dpkg-query -W -f '${Package} ${Version}\n' mergerfs | tee -a "$OUT/mergerfs-options.log"

OPTS="category.create=mspmfs,moveonenospc=true,dropcacheonclose=true,minfreespace=50M,fsname=hoserva-s8,cache.files=partial,cache.statfs=0,cache.entry=1,cache.attr=1,cache.negative_entry=1"
echo "== mount command ==" | tee -a "$OUT/mergerfs-options.log"
CMD=(mergerfs -f -o "$OPTS" "$LAB/mnt/disk1:$LAB/mnt/disk2:$LAB/mnt/disk3" "$MNT")
echo "+ ${CMD[*]}" | tee -a "$OUT/mergerfs-options.log"
"${CMD[@]}" >"$OUT/mergerfs-fg.log" 2>&1 &
PID=$!
echo "pid=$PID" | tee -a "$OUT/mergerfs-options.log"

# wait for the mount to actually appear rather than a fixed sleep
for _ in $(seq 1 50); do
  mountpoint -q "$MNT" && break
  sleep 0.2
done
mountpoint -q "$MNT" || { echo "mount did not come up" >&2; cat "$OUT/mergerfs-fg.log" >&2; exit 1; }

echo "== findmnt (fsname / mount-reported view) ==" | tee -a "$OUT/mergerfs-options.log"
findmnt -no SOURCE,FSTYPE,OPTIONS "$MNT" | tee -a "$OUT/mergerfs-options.log" "$OUT/findmnt.log"

echo "== full runtime control-file dump: getfattr -d \$MNT/.mergerfs ==" | tee -a "$OUT/mergerfs-options.log"
getfattr -d "$MNT/.mergerfs" 2>&1 | tee -a "$OUT/mergerfs-options.log" "$OUT/getfattr-full.log"

echo "== individual doc 02 §1 keys, read one at a time ==" | tee -a "$OUT/mergerfs-options.log"
for key in category.create moveonenospc dropcacheonclose minfreespace \
           cache.files cache.statfs cache.entry cache.attr cache.negative_entry \
           branches; do
  echo "-- user.mergerfs.$key --" | tee -a "$OUT/mergerfs-options.log"
  getfattr -n "user.mergerfs.$key" "$MNT/.mergerfs" 2>&1 | tee -a "$OUT/mergerfs-options.log"
done

echo "== create-policy round-trip: setfattr then getfattr, for every doc 02 §1 policy ==" | tee -a "$OUT/mergerfs-options.log"
for policy in mspmfs mfs lfs ff epmfs; do
  setfattr -n user.mergerfs.category.create -v "$policy" "$MNT/.mergerfs"
  actual=$(getfattr -n user.mergerfs.category.create --only-values "$MNT/.mergerfs" 2>/dev/null)
  echo "set=$policy reported=$actual" | tee -a "$OUT/mergerfs-options.log" "$OUT/policy-roundtrip.log"
  [[ "$actual" == "$policy" ]] || { echo "MISMATCH: set $policy, mergerfs reports $actual" >&2; exit 1; }
done
# restore mspmfs (this spike's own default, doc 02 §1) before the behavioural
# checks below, which assume it
setfattr -n user.mergerfs.category.create -v mspmfs "$MNT/.mergerfs"

echo "== a file written through the mount, to confirm the mount is genuinely live, not just reporting xattrs ==" | tee -a "$OUT/mergerfs-options.log"
echo "s8-probe" > "$MNT/s8-probe.txt"
sync
for d in disk1 disk2 disk3; do
  if [[ -e "$LAB/mnt/$d/s8-probe.txt" ]]; then
    echo "probe landed on: $d" | tee -a "$OUT/mergerfs-options.log"
  fi
done
diff <(echo "s8-probe") <(cat "$MNT/s8-probe.txt") && echo "probe read-back matches" | tee -a "$OUT/mergerfs-options.log"

echo "== cache.files effective mount-option is also visible via /proc mountinfo super options ==" | tee -a "$OUT/mergerfs-options.log"
grep -F "$MNT" /proc/self/mountinfo | tee -a "$OUT/mergerfs-options.log" "$OUT/mountinfo.log"

echo "== teardown: unmount and confirm the pid exits cleanly ==" | tee -a "$OUT/mergerfs-options.log"
fusermount3 -u "$MNT"
for _ in $(seq 1 50); do
  kill -0 "$PID" 2>/dev/null || break
  sleep 0.2
done
if kill -0 "$PID" 2>/dev/null; then
  echo "WARNING: pid $PID still alive after unmount" | tee -a "$OUT/mergerfs-options.log"
else
  echo "pid $PID exited cleanly after unmount" | tee -a "$OUT/mergerfs-options.log"
fi
rmdir "$MNT"

echo "OK" | tee -a "$OUT/mergerfs-options.log"
