#!/usr/bin/env bash
# Tear this lab down completely, from inside the container.
#
# Everything the lab writes under $LAB is owned by root, so it can only be
# removed from in here: once the container is gone, the developer's own user
# cannot delete it, and an agent's scratch clone holding a leftover .lab/
# cannot be deleted either (spike S9, doc 08; issue #120).
#
# Order matters: unmount, then detach, then delete. Deleting an image while
# its loop device is still attached leaves a device whose backing file reads
# "(deleted)" — and once the path is gone, nothing can attribute that device
# to a lab any more, so no later cleanup can safely remove it.
set -euo pipefail
HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source "$HERE/lib.sh"
lab_require_id

unmount_if_mounted() {
  local m=$1
  mountpoint -q "$m" 2>/dev/null || return 0
  local i
  for i in 1 2 3 4 5; do
    if fusermount -u "$m" 2>/dev/null || umount "$m" 2>/dev/null; then
      return 0
    fi
    mountpoint -q "$m" 2>/dev/null || return 0
    sleep 1
  done
  fusermount -uz "$m" 2>/dev/null || umount -l "$m" 2>/dev/null || true
  if mountpoint -q "$m" 2>/dev/null; then
    die "failed to unmount $m: still busy after retries and a lazy unmount — refusing to continue (the container must stay up so its root-owned files under \$LAB remain reachable; free the mount and retry)"
  fi
}

# Every mount strictly *below* $LAB, deepest first, so a per-share mergerfs
# mount nested inside the catch-all is unmounted before the thing it sits on.
# findmnt --submounts reports $LAB itself too — it is the container's own
# bind mount from the host, not one of this lab's array mounts — so it is
# excluded here rather than unmounted: unmounting it left the rest of this
# script operating on an empty overlay directory that could never fail its
# own checks (issue #122). The old code only knew about $LAB/mnt/user and
# $LAB/mnt/<disk>, which is why spikes that mounted anywhere else left mounts
# (and therefore images, and therefore loop devices) behind.
while IFS= read -r m; do
  [[ -n "$m" && "$m" != "$LAB" ]] || continue
  unmount_if_mounted "$m"
done < <(findmnt --raw --noheadings --output TARGET --submounts "$LAB" 2>/dev/null \
          | awk '{ print length($0), $0 }' | sort -rn | cut -d' ' -f2- || true)

# Detach by asking the kernel which loop devices are backed by a path under
# this lab, rather than by looking for *.img files that still exist. An image
# a script created outside $LAB/img, or one it already deleted, was invisible
# to the old find-based loop — the gap that left orphans behind (issue #120).
unattributable=()
for backing in /sys/block/loop*/loop/backing_file; do
  [[ -e "$backing" ]] || continue
  dev="/dev/$(basename "$(dirname "$(dirname "$backing")")")"
  path=$(cat "$backing" 2>/dev/null) || continue
  [[ -n "$path" ]] || continue

  # The kernel appends " (deleted)" once the backing file is unlinked. Strip
  # it for the prefix test: a device whose path still names this lab is ours
  # to detach even if the file behind it is already gone.
  stripped=${path% (deleted)}

  case "$stripped" in
    "$LAB"/*)
      losetup -d "$dev" 2>/dev/null || die "failed to detach $dev (backed by $path)"
      ;;
    *)
      # Not ours by path. A device whose backing file is deleted can never be
      # attributed to any lab again, so it is reported, never detached:
      # guessing here would mean detaching a global device on a shared host
      # on the strength of a name that no longer resolves.
      [[ "$path" == *" (deleted)" ]] && unattributable+=("$dev -> $path")
      ;;
  esac
done

# The whole point of deleting from *inside* the container is that $LAB is the
# container's bind mount to the host's .lab/<id> — a delete that ran against
# an unmounted $LAB would land on the overlay's own empty directory instead,
# pass every check below on nothing, and leave every root-owned file behind
# on the host (exactly issue #122). Assert the mount survived the unmount
# loop above before trusting anything it finds empty.
mountpoint -q "$LAB" || die "\$LAB ($LAB) is not a mountpoint before delete — refusing to run rm -rf against whatever is there instead of the lab's bind mount. This is the exact state that silently stranded root-owned files before (issue #122); the mount must be restored (or the container recreated) before teardown can proceed."

# Remove the lab's contents, not the directory itself: $LAB is the container's
# bind mount, so unlinking it here would not remove it on the host anyway.
# Everything, not just img/ and mnt/ — a results directory or scratch tree a
# spike wrote is equally root-owned and equally undeletable from outside.
# "${LAB:?}" rather than "$LAB": this is a recursive delete running as root
# inside the container, so an empty LAB must abort the expansion rather than
# become `rm -rf /*`. lab_require_id already guarantees LAB is set — the
# guard is here because that guarantee is not what should be standing
# between root and the filesystem root.
shopt -s dotglob nullglob
rm -rf -- "${LAB:?}"/*
shopt -u dotglob nullglob

remaining=$(find "$LAB" -mindepth 1 -maxdepth 1 2>/dev/null | head -20 || true)
if [[ -n "$remaining" ]]; then
  die "lab $HOSERVA_LAB_ID: these are still under \$LAB after teardown, so the host cannot delete .lab/$HOSERVA_LAB_ID:
$remaining"
fi

echo "lab $HOSERVA_LAB_ID destroyed"

if (( ${#unattributable[@]} > 0 )); then
  printf 'lab %s: WARNING — loop devices with a deleted backing file, not attributable to any lab and therefore NOT detached:\n' "$HOSERVA_LAB_ID" >&2
  printf '  %s\n' "${unattributable[@]}" >&2
  printf 'lab %s: clear them deliberately (`sudo losetup -d <device>`) once you know what they were.\n' "$HOSERVA_LAB_ID" >&2
fi
