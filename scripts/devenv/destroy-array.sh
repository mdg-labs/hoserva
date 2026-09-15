#!/usr/bin/env bash
# Tears down this lab's array from inside the container: unmounts the pool
# and every per-disk mount, detaches only the loop devices backed by this
# lab's own images (resolved via `losetup -j`, never `losetup -D`), then
# deletes $LAB — from inside, because everything the lab creates is
# root-owned in the bind mount and the host user cannot remove it after the
# container exits (doc 08, S9). Idempotent: safe to run against a half-built
# or already-torn-down lab, and never touches another lab's devices.
#
# If a mount cannot be freed even after a retrying, escalating unmount, this
# exits non-zero instead of falling through to `rm -rf`: `make lab-destroy`
# only removes the container once this script succeeds, specifically so a
# genuinely stuck mount never gets the container (the only thing with
# permission on these root-owned files) pulled out from under it.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"
lab_require_id

unmount_if_mounted() {
  local m=$1
  mountpoint -q "$m" 2>/dev/null || return 0

  # A mount held busy for a moment (e.g. a shell cd'd into it) often clears
  # on its own — retry a graceful unmount a few times before escalating.
  local i
  for i in 1 2 3 4 5; do
    if fusermount -u "$m" 2>/dev/null || umount "$m" 2>/dev/null; then
      return 0
    fi
    mountpoint -q "$m" 2>/dev/null || return 0
    sleep 1
  done

  # Still busy: a lazy unmount detaches the mountpoint from the namespace
  # immediately even though the busy holder keeps its own reference — this
  # is what actually clears the path for the `rm -rf` below, rather than
  # leaving it to fail underneath a container that has just been removed.
  fusermount -uz "$m" 2>/dev/null || umount -l "$m" 2>/dev/null || true

  if mountpoint -q "$m" 2>/dev/null; then
    die "failed to unmount $m: still busy after retries and a lazy unmount — refusing to continue (the container must stay up so its root-owned files under \$LAB remain reachable; free the mount and retry)"
  fi
}

unmount_if_mounted "$LAB/mnt/user"

if [[ -d "$LAB/mnt" ]]; then
  for m in "$LAB"/mnt/*/; do
    [[ -d "$m" ]] || continue
    unmount_if_mounted "${m%/}"
  done
fi

# Detach exactly the loop devices whose backing file is one of our own
# images — resolved per image via `losetup -j`, so a device this lab never
# attached, or another lab's device that happens to share a loop number on
# this host, is never touched.
if [[ -d "$LAB/img" ]]; then
  while IFS= read -r img; do
    [[ -f "$img" ]] || continue
    while IFS= read -r dev; do
      [[ -n "$dev" ]] || continue
      losetup -d "$dev" 2>/dev/null || true
    done < <(losetup -j "$img" --output NAME --noheadings 2>/dev/null || true)
  done < <(find "$LAB/img" -maxdepth 1 -type f -name '*.img')
fi

# Delete the contents from inside the container: everything under here was
# created as root in here, so the host user cannot remove it once the
# container exits (doc 08, S9). $LAB itself is the bind-mount target the
# host created before starting the container, so it is left for `make
# lab-destroy` to remove from the host side — rmdir-ing a live mountpoint
# from inside would just fail.
rm -rf -- "$LAB/img" "$LAB/mnt"

echo "lab $HOSERVA_LAB_ID destroyed"
