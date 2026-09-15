# Shared helpers for spike S5 (SnapRAID fidelity on loop devices). Sourced,
# not executed.
#
# This spike runs against the *standing* array `make lab-up` built (this
# lab's own geometry: LAB_DATA_SIZE=2G, LAB_PARITY_SIZE=3G — see
# spikes/s5/README.md for why), reusing $LAB/mnt/parity1, disk1..3 and cache
# directly rather than a separate subtree — SnapRAID's own working set
# genuinely is those mount points. This spike's *own* files (a second
# parity disk, replacement disks, the snapraid.conf, its content/parity
# files under the same mounts, and every log/manifest this run produces)
# live under $LAB/img (extra images, alongside the ones create-array.sh
# made — same directory, same lab_assert_own_loop discipline) and a
# dedicated $LAB/s5 subtree for everything that is not an image or a mount
# (conf, logs, manifests, fix logs) — which is not cleaned by
# destroy-array.sh (it only removes $LAB/img and $LAB/mnt) and so is this
# spike's own responsibility to remove from inside the container before
# `make lab-destroy` (dispatch teardown instructions).
#
# Loop-device discipline mirrors scripts/devenv/lib.sh exactly: every device
# this spike attaches is resolved back to one of its own image files via
# `losetup -j` before use, and `losetup -D` (detach-all) is never used.

# shellcheck source=/dev/null
source /src/scripts/devenv/lib.sh
lab_require_id

S5="$LAB/s5"
CONF="$S5/snapraid.conf"
mkdir -p -- "$S5"

s5_log() { printf '%s\n' "$*"; }

# Runs snapraid against this spike's config file, printing the exact
# command and its exit status before and after — every claim in doc 08
# about "sync exited 0" or "diff returned 2" must be checkable against a
# committed log line that actually shows the command and the code, not
# prose asserting it happened.
s5_snapraid() {
  echo "+ snapraid -c $CONF $*"
  local rc=0
  snapraid -c "$CONF" "$@" || rc=$?
  echo "[exit $rc]"
  return "$rc"
}

# Runs snapraid, tees its output to both stdout and a log file, and leaves
# the real exit code in $S5_LAST_RC — without a pipe, since under
# `set -euo pipefail` a pipeline ending in `snapraid diff`'s expected exit 2
# (or any non-zero) aborts the script at `set -e` before PIPESTATUS can ever
# be read, and `|| true` on the pipeline itself resets PIPESTATUS to that of
# the fallback, not the original command (confirmed empirically while
# writing this spike: `f() { return 2; }; f | cat || true; echo
# $PIPESTATUS` prints 0, not 2). Plain redirection sidesteps the whole
# interaction.
s5_snapraid_log() {
  local logfile=$1; shift
  set +e
  s5_snapraid "$@" > "$logfile" 2>&1
  S5_LAST_RC=$?
  set -e
  cat "$logfile"
}

# name, size -> creates $LAB/img/<name>.img, attaches it, XFS-formats and
# mounts at $LAB/mnt/<name> — identical recipe to create-array.sh's own
# create_disk(), reused here (not reimplemented) for the extra disks this
# spike needs (a second parity disk, and fresh replacement disks for the
# reconstruction tests) that create-array.sh itself does not build.
s5_create_disk() {
  local name=$1 size=$2
  local img="$LAB/img/$name.img"
  [[ -e "$img" ]] && die "refusing to recreate existing image: $img"
  truncate -s "$size" "$img"
  local dev
  dev=$(losetup --find --show "$img")
  lab_assert_own_loop "$dev" "$img"
  mkfs.xfs -q -L "$name" "$dev"
  mkdir -p -- "$LAB/mnt/$name"
  mount "$dev" "$LAB/mnt/$name"
  echo "created $name: image=$img dev=$dev mount=$LAB/mnt/$name"
}

# Detaches the loop device backing $LAB/mnt/<name> and unmounts it, without
# deleting the image — used for the "pull a disk" failure-injection step.
# Confirms the device is backed by our own image immediately before the
# detach, exactly as CLAUDE.md's corruption-injection rule requires.
s5_detach_disk() {
  local name=$1
  local mnt="$LAB/mnt/$name" img="$LAB/img/$name.img"
  local dev
  dev=$(findmnt -n -o SOURCE --target "$mnt") || die "no mount at $mnt"
  lab_assert_own_loop "$dev" "$img"
  umount "$mnt"
  losetup -d "$dev"
  echo "detached $name: dev=$dev (image $img left in place, disk now 'pulled')"
}

# sha256 of every regular file under a data disk's mount, one line
# "hash  disk/relpath" per file, sorted — the raw material every
# reconstruction claim in this spike is checked against, never eyeballed.
s5_disk_manifest() {
  local name=$1 mnt="$LAB/mnt/$1"
  ( cd -- "$mnt" && find . -type f -not -name 'snapraid.*' -printf '%P\n' | sort | while IFS= read -r f; do
      sha256sum -- "$f" | awk -v d="$name" '{print $1"  "d"/"$2}'
    done )
}

# Concatenated, sorted manifest across disk1..disk3 (and any extra data
# disk named as an argument) — one file this spike compares before/after
# every failure-injection step.
s5_array_manifest() {
  local d
  for d in "$@"; do s5_disk_manifest "$d"; done | sort -k2
}
