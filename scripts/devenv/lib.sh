# Shared safety guards for the loop-device lab (doc 06 §3, Q45).
#
# Sourced, not executed. Every other script in this directory works only
# under its own $LAB/img/, resolves every device it touches through
# `losetup -j` against a path it built itself, and never widens scope with
# `losetup -D` (detach all) or a device path taken from anywhere but that
# resolution. This file is the one place that logic lives, so it is checked
# once instead of trusted to be repeated correctly five times.

die() { printf 'lab: %s\n' "$*" >&2; exit 1; }

# Same shape a lab id must have everywhere it is used (Makefile's
# lab-require-id target enforces the identical pattern) — reject anything
# that could turn a directory name into a shell glob or a path escape before
# it ever reaches `rm -rf`, `losetup`, a container name or a bind mount.
lab_id_valid() {
  local id=$1
  [[ "$id" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || return 1
  [[ "$id" != *..* ]] || return 1
  return 0
}

# Sets LAB to this lab's own directory and requires the id that namespaces
# every image, mount point and container name (Q45) — never a shared path.
lab_require_id() {
  : "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID — parallel labs must never share loop devices or paths}"
  lab_id_valid "$HOSERVA_LAB_ID" || die "invalid HOSERVA_LAB_ID '$HOSERVA_LAB_ID': must match ^[a-zA-Z0-9][a-zA-Z0-9_.-]*\$ and must not contain '..'"
  LAB="/lab/$HOSERVA_LAB_ID"
  mkdir -p -- "$LAB/img" "$LAB/mnt"
}

# Refuses any device that is not the loop device losetup itself just attached
# to one of our own images. A device path is never accepted on trust: it is
# only ever used after this resolves it back to an image under our own
# $LAB/img/, so a bug that produced /dev/sda here would be refused, not run.
#
# The $LAB/img/ prefix check is done on the *canonical* form of both sides:
# a plain string-prefix/glob comparison would let a path containing `..`
# (e.g. "$LAB/img/../../x.img") through, since a glob's `*` also matches
# `/`. realpath -m collapses `..` segments first — without requiring the
# path to exist — so the comparison that follows cannot be fooled by one.
lab_assert_own_loop() {
  local dev=$1 img=$2
  local resolved_img resolved_lab_img
  resolved_img=$(realpath -m -- "$img") || die "refusing $img: cannot resolve path"
  resolved_lab_img=$(realpath -m -- "$LAB/img")
  case "$resolved_img" in
    "$resolved_lab_img"/*) ;;
    *) die "refusing $img: not under \$LAB/img/" ;;
  esac
  case "$dev" in
    /dev/loop[0-9]*) ;;
    *) die "refusing non-loop device: $dev" ;;
  esac
  local resolved
  resolved=$(losetup -j "$resolved_img" --output NAME --noheadings 2>/dev/null | tr -d '[:space:]')
  [[ -n "$resolved" && "$resolved" == "$dev" ]] || die "refusing $dev: not backed by $img"
}

# Unmounts one mount, retrying while it is busy. A mount still busy after the
# retries is lazily unmounted as a last resort, and only a mount that is
# *still* a mountpoint after that dies: the container has to stay up so the
# lab's root-owned files stay reachable.
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

# Frees one loop device whose backing file is on a lab mount: unmounts what is
# mounted from it (deepest first), detaches it, and dies unless the kernel no
# longer lists it. A detach that only marks a still-busy device autoclear
# would otherwise pass for done and leave it holding its backing file's mount.
_lab_free_nested_loop() {  # device backing-path
  local dev=$1 path=$2 t
  while IFS= read -r t; do
    [[ -n "$t" ]] || continue
    unmount_if_mounted "$t"
  done < <(findmnt --raw --noheadings --output TARGET --source "$dev" 2>/dev/null \
            | awk '{ print length($0), $0 }' | sort -rn | cut -d' ' -f2- || true)
  losetup -d "$dev" 2>/dev/null || die "failed to detach $dev (backed by $path)"
  if [[ -e "/sys/block/${dev#/dev/}/loop/backing_file" ]]; then
    die "$dev (backed by $path) is still attached after losetup -d — refusing to continue (the container must stay up so the device stays reachable; free what holds it and retry)"
  fi
}

# Detaches every loop device whose backing file lies on one of this lab's own
# array mounts (a mount strictly below $LAB), while those mounts are still
# attached. Such a device holds its backing file's mount busy, so the unmount
# that follows would fall back to a lazy unmount; the kernel then reports the
# device's backing path relative to the detached filesystem, no longer under
# $LAB, and nothing could attribute the device to this lab again (issue #576).
#
# Attribution is by path under this lab's own mounts only. $LAB itself is the
# container's bind mount, so the devices backing the lab's own images
# ($LAB/img) are not matched here — they are detached once everything is
# unmounted. A device whose filesystem holds another candidate's backing file
# goes after that candidate.
lab_detach_nested_loops() {
  local -a mounts=() cand_dev=() cand_path=() rest_dev=() rest_path=()
  local m backing dev path stripped i j t hosts progressed
  while IFS= read -r m; do
    if [[ -n "$m" && "$m" != "$LAB" ]]; then mounts+=("$m"); fi
  done < <(findmnt --raw --noheadings --output TARGET --submounts "$LAB" 2>/dev/null || true)

  for backing in /sys/block/loop*/loop/backing_file; do
    [[ -e "$backing" ]] || continue
    dev="/dev/$(basename "$(dirname "$(dirname "$backing")")")"
    path=$(cat "$backing" 2>/dev/null) || continue
    stripped=${path% (deleted)}
    for m in "${mounts[@]}"; do
      if [[ "$stripped" == "$m"/* ]]; then
        cand_dev+=("$dev")
        cand_path+=("$path")
        break
      fi
    done
  done

  while ((${#cand_dev[@]} > 0)); do
    progressed=0
    rest_dev=()
    rest_path=()
    for i in "${!cand_dev[@]}"; do
      hosts=0
      while IFS= read -r t; do
        [[ -n "$t" ]] || continue
        for j in "${!cand_dev[@]}"; do
          if [[ "$j" != "$i" && "${cand_path[$j]%" (deleted)"}" == "$t"/* ]]; then
            hosts=1
          fi
        done
      done < <(findmnt --raw --noheadings --output TARGET --source "${cand_dev[$i]}" 2>/dev/null || true)
      if ((hosts)); then
        rest_dev+=("${cand_dev[$i]}")
        rest_path+=("${cand_path[$i]}")
      else
        _lab_free_nested_loop "${cand_dev[$i]}" "${cand_path[$i]}"
        progressed=1
      fi
    done
    ((progressed)) || die "loop devices ${rest_dev[*]} each hold another's backing file — cannot order their detach"
    cand_dev=("${rest_dev[@]}")
    cand_path=("${rest_path[@]}")
  done
}
