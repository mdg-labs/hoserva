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
