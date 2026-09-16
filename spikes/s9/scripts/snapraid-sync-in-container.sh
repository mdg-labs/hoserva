#!/usr/bin/env bash
# Runs *inside* the lab container (invoked by hosted-snapraid-check.sh via
# `docker compose exec`). Installs snapraid for this spike only, builds a
# throwaway config against the standing array's own mount points, and runs
# one sync — the same commands this spike validated by hand in the local
# lab (lab id 10-a1, 2026-09-16, spikes/s9/results/local-run/sync.log)
# before this script existed, reused here rather than reimplemented.
set -euo pipefail

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"
LAB="/lab/$HOSERVA_LAB_ID"
S9="$LAB/s9"

[[ -d "$LAB/mnt/disk1" ]] || { echo "no standing array at $LAB — run lab-up first" >&2; exit 1; }

apt-get update -qq
apt-get install -y -qq --no-install-recommends snapraid >/dev/null
dpkg-query -W -f 'snapraid ${Version}\n' snapraid

mkdir -p -- "$S9"
cat > "$S9/snapraid.conf" <<EOF
parity $LAB/mnt/parity1/parity
content $S9/content
content $LAB/mnt/disk1/.snapraid.content
content $LAB/mnt/disk2/.snapraid.content
data d1 $LAB/mnt/disk1
data d2 $LAB/mnt/disk2
data d3 $LAB/mnt/disk3
EOF

# A probe file per disk, distinct from whatever lab-seed already put there
# (lab-seed's own files are enough for snapraid to have something to sync,
# but a named probe file makes the sync's own "what changed" visible in the
# log without having to cross-reference lab-seed's profile).
mkdir -p -- "$LAB/mnt/disk1/s9-probe" "$LAB/mnt/disk2/s9-probe"
echo "s9-probe-file-1" > "$LAB/mnt/disk1/s9-probe/a.txt"
echo "s9-probe-file-2" > "$LAB/mnt/disk2/s9-probe/b.txt"

echo "+ snapraid -c $S9/snapraid.conf sync"
rc=0
snapraid -c "$S9/snapraid.conf" sync > "$S9/sync.log" 2>&1 || rc=$?
echo "[exit $rc]"
cat "$S9/sync.log"

if [[ $rc -ne 0 ]]; then
  echo "snapraid sync failed (exit $rc) — see log above" >&2
  exit "$rc"
fi

grep -q '^Everything OK$' "$S9/sync.log" || { echo "sync exited 0 but 'Everything OK' was not printed — treat as unconfirmed, not passed" >&2; exit 1; }

echo "snapraid sync: confirmed (exit 0, 'Everything OK')"

# Cleanup: destroy-array.sh only removes $LAB/img and $LAB/mnt's own disk
# images, not files this spike added under existing mount points or $LAB/s9
# itself — leaving them would make lab-destroy's teardown incomplete for
# a next run reusing the same id.
rm -rf -- "$S9" "$LAB/mnt/disk1/s9-probe" "$LAB/mnt/disk2/s9-probe" \
  "$LAB/mnt/disk1/.snapraid.content" "$LAB/mnt/disk2/.snapraid.content"
