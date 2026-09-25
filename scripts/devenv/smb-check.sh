#!/usr/bin/env bash
# Starts smbd inside the lab against a Hoserva-generated smb.conf and
# exercises the mutual-visibility proxy #48's own AC4 needs (issue #219): a
# file written over SMB as uid:gid 99:100 (Q26) is readable on the plain
# filesystem as that same identity, and a file written directly on the
# filesystem as that identity is readable back over SMB.
#
# The smb.conf under test is scripts/devenv/gen-smb-conf's own call into
# internal/config.RenderSambaConf, rendered on the host by
# `make test-integration` into $LAB/smb.conf.rendered (this container has no
# Go toolchain) — never a hand-written fixture. RenderSambaConf now emits
# #48's own Q26 masks and force modes on every share (`create mask = 0664`
# with `force create mode = 0664`, `directory mask = 2775` with `force
# directory mode = 2775`, `force group = users`); the assertions below
# still only check uid:gid ownership and content, not exact mode bits, so
# they hold against the new output without needing a value change of
# their own.
#
# Run only inside the lab container, via `make test-integration`.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"
lab_require_id

command -v smbd >/dev/null 2>&1 \
  || die "smbd is not installed in this image — it is expected to be (scripts/devenv/Dockerfile)"
command -v smbclient >/dev/null 2>&1 \
  || die "smbclient is not installed in this image — it is expected to be (scripts/devenv/Dockerfile)"
command -v smbpasswd >/dev/null 2>&1 \
  || die "smbpasswd is not installed in this image — it is expected to be (scripts/devenv/Dockerfile)"

[[ -d "$LAB/mnt/user" ]] || die "no pool mount at \$LAB/mnt/user — run create-array.sh first"

rendered="$LAB/smb.conf.rendered"
[[ -f "$rendered" ]] || die "missing $rendered — run this via 'make test-integration', which renders it on the host first"

SHARE=smbcheck
TEST_UID=99
TEST_GID=100
PASSWORD="hoserva-lab-check"

work=""
extra_globals=""
smbd_pid=""
mnt_user_created=0
cleanup() {
  # smbd's own process group, not just its own PID: it forks smbd-notifyd
  # and smbd-cleanupd helpers into the same group, and leaving them running
  # would leak past this script even though the master exited.
  [[ -n "$smbd_pid" ]] && kill -- "-$smbd_pid" 2>/dev/null
  [[ -n "$extra_globals" ]] && rm -f "$extra_globals"
  [[ -n "$work" ]] && rm -rf "$work"
  # Only ever remove the /mnt/user symlink this run created itself (issue
  # #363): a stray leak here masquerades, to any later lab test that mounts
  # pool.CatchAllPath fresh, as an existing non-hoserva-pool mount at that
  # path. -L (not -e) because a symlink whose target this same cleanup may
  # be racing to tear down elsewhere must still be recognised and removed.
  if [[ "$mnt_user_created" -eq 1 && -L /mnt/user ]]; then
    rm -f /mnt/user
  fi
  return 0
}
trap cleanup EXIT

# /mnt/user is the pool path RenderSambaConf's share sections hard-code
# (doc 03 §4.2) — a plain symlink onto this lab's own mergerfs mount
# (create-array.sh), so the rendered path resolves without touching
# anything outside this container. Only set mnt_user_created when this run
# is the one making the symlink: a path already occupied here is left
# exactly as found, and cleanup above must not remove something it didn't
# create (issue #363).
mkdir -p /mnt
if [[ -e /mnt/user ]]; then
  echo "smb-check: /mnt/user already exists — leaving it as found, not removing it on exit"
else
  ln -s "$LAB/mnt/user" /mnt/user
  mnt_user_created=1
fi

# The user-owned escape hatch every generated smb.conf ends with
# (SambaCustomInclude, internal/config/samba.go) — empty is enough for smbd
# to parse the include successfully; Hoserva itself never writes to it.
mkdir -p /etc/hoserva
: > /etc/hoserva/smb.custom.conf

share_dir="/mnt/user/$SHARE"
mkdir -p "$share_dir"

group_name=$(getent group "$TEST_GID" | cut -d: -f1 || true)
if [[ -z "$group_name" ]]; then
  group_name=hoserva-smbcheck
  groupadd -g "$TEST_GID" "$group_name"
fi

user_name=$(getent passwd "$TEST_UID" | cut -d: -f1 || true)
if [[ -z "$user_name" ]]; then
  user_name=hoserva-smbcheck
  useradd -M -N -s /usr/sbin/nologin -u "$TEST_UID" -g "$TEST_GID" "$user_name"
fi

# Directory provisioning so the connecting Unix account (uid $TEST_UID, gid
# $TEST_GID) can write into the share at all — unrelated to smb.conf's own
# masks, which stay exactly what RenderSambaConf emits above.
chown "root:$TEST_GID" "$share_dir"
chmod 0770 "$share_dir"

mkdir -p "$LAB/samba/private" "$LAB/samba/lock" "$LAB/samba/run" "$LAB/samba/log"
# smbd's RPC pipe directory is not one of the state paths it takes from
# smb.conf's own parameters — this is the container's own ephemeral /run,
# not a host or workspace path.
mkdir -p /run/samba

# smbd needs somewhere to keep its own state; splicing these paths in after
# [global] keeps every share-relevant line (security, vfs objects, the
# share's own settings) exactly what RenderSambaConf produced above — this
# only tells smbd where to put its private/lock/pid/log files instead of the
# image's system paths.
extra_globals=$(mktemp)
{
  echo "   private dir = $LAB/samba/private"
  echo "   lock directory = $LAB/samba/lock"
  echo "   state directory = $LAB/samba/lock"
  echo "   cache directory = $LAB/samba/lock"
  echo "   pid directory = $LAB/samba/run"
  echo "   log file = $LAB/samba/log/smbd.log"
  echo "   passdb backend = tdbsam:$LAB/samba/private/passdb.tdb"
  echo "   bind interfaces only = yes"
  echo "   interfaces = lo"
} > "$extra_globals"

conf="$LAB/samba/smb.conf"
sed -e "/^\[global\]\$/r $extra_globals" "$rendered" > "$conf"

echo "## smb.conf under test (RenderSambaConf's output, lab-only state paths spliced in after [global]):"
cat "$conf"

if ! pdbedit -s "$conf" -L 2>/dev/null | grep -q "^$user_name:"; then
  printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | smbpasswd -c "$conf" -a -s "$user_name" >/dev/null
fi

# World-readable/writable: setpriv below drops to uid $TEST_UID to write
# and read as that identity, and needs to reach files staged here by root.
work=$(mktemp -d)
chmod 0777 "$work"

# -F (foreground, no double-fork) so $! is smbd's own PID. Its own process
# group (never suppressed with --no-process-group) is what lets cleanup
# above tear down its notifyd/cleanupd helpers via that PID's negated
# group, without also signalling this script's own shell.
smbd -s "$conf" -F &
smbd_pid=$!

connected=0
for _ in $(seq 1 40); do
  if (exec 3<>"/dev/tcp/127.0.0.1/445") 2>/dev/null; then
    exec 3>&- 3<&-
    connected=1
    break
  fi
  sleep 0.25
done
[[ "$connected" -eq 1 ]] || die "smbd did not start listening on 127.0.0.1:445 within 10s — see $LAB/samba/log/smbd.log"

# --- Direction 1: write over SMB, read on the plain filesystem as 99:100 ---
via_smb_content="hoserva smb-check via SMB $(date +%s%N)"
printf '%s' "$via_smb_content" > "$work/via-smb.txt"

smbclient "//127.0.0.1/$SHARE" -U "$user_name%$PASSWORD" \
  -c "put $work/via-smb.txt via-smb.txt" \
  || die "smbclient put failed — see $LAB/samba/log/smbd.log"

owner=$(stat -c '%u:%g' "$share_dir/via-smb.txt")
[[ "$owner" == "$TEST_UID:$TEST_GID" ]] \
  || die "file written over SMB is owned $owner, want $TEST_UID:$TEST_GID"
echo "ok: file written over SMB is owned $owner"

fs_read=$(setpriv --reuid "$TEST_UID" --regid "$TEST_GID" --clear-groups \
  cat "$share_dir/via-smb.txt")
[[ "$fs_read" == "$via_smb_content" ]] \
  || die "plain filesystem read as $TEST_UID:$TEST_GID did not see the content written over SMB"
echo "ok: plain filesystem read as $TEST_UID:$TEST_GID sees the content written over SMB"

# Overwrite of the SMB-created file, on the plain filesystem as 99:100 —
# the masks only guarantee a *new* file lands group-writable; this is what
# actually proves an existing Samba-owned file stays writable by the
# shared identity afterward.
via_smb_overwrite="hoserva smb-check via SMB, overwritten on the filesystem $(date +%s%N)"
setpriv --reuid "$TEST_UID" --regid "$TEST_GID" --clear-groups \
  tee "$share_dir/via-smb.txt" >/dev/null <<<"$via_smb_overwrite" \
  || die "overwriting via-smb.txt as $TEST_UID:$TEST_GID failed"
fs_reread=$(setpriv --reuid "$TEST_UID" --regid "$TEST_GID" --clear-groups \
  cat "$share_dir/via-smb.txt")
[[ "$fs_reread" == "$via_smb_overwrite" ]] \
  || die "plain filesystem read as $TEST_UID:$TEST_GID did not see its own overwrite of the SMB-created file"
echo "ok: $TEST_UID:$TEST_GID overwrote the SMB-created file on the plain filesystem"

# --- Direction 2: write on the plain filesystem as 99:100, read over SMB ---
via_fs_content="hoserva smb-check via filesystem $(date +%s%N)"
printf '%s' "$via_fs_content" > "$work/via-fs.txt"
setpriv --reuid "$TEST_UID" --regid "$TEST_GID" --clear-groups \
  cp "$work/via-fs.txt" "$share_dir/via-fs.txt" \
  || die "writing as $TEST_UID:$TEST_GID on the plain filesystem failed"
echo "ok: file written on the plain filesystem as $TEST_UID:$TEST_GID"

smb_read="$work/via-fs.roundtrip.txt"
smbclient "//127.0.0.1/$SHARE" -U "$user_name%$PASSWORD" \
  -c "get via-fs.txt $smb_read" \
  || die "smbclient get failed — see $LAB/samba/log/smbd.log"
diff -q "$work/via-fs.txt" "$smb_read" >/dev/null \
  || die "SMB read did not see the content written on the plain filesystem"
echo "ok: SMB read as $user_name sees the content written on the plain filesystem as $TEST_UID:$TEST_GID"

# Overwrite of the filesystem-created file, over SMB — the mirror of the
# direction-1 overwrite above: proves the SMB user can write back into a
# file the plain filesystem identity already owns, not just create fresh
# ones.
via_fs_overwrite="hoserva smb-check via filesystem, overwritten over SMB $(date +%s%N)"
printf '%s' "$via_fs_overwrite" > "$work/via-fs.overwrite.txt"
smbclient "//127.0.0.1/$SHARE" -U "$user_name%$PASSWORD" \
  -c "put $work/via-fs.overwrite.txt via-fs.txt" \
  || die "smbclient overwrite of via-fs.txt failed — see $LAB/samba/log/smbd.log"
fs_overwrite_read=$(setpriv --reuid "$TEST_UID" --regid "$TEST_GID" --clear-groups \
  cat "$share_dir/via-fs.txt")
[[ "$fs_overwrite_read" == "$via_fs_overwrite" ]] \
  || die "plain filesystem read as $TEST_UID:$TEST_GID did not see the SMB overwrite of via-fs.txt"
echo "ok: SMB overwrite of the filesystem-created file is visible on the plain filesystem"

echo "smb-check: confirmed (Hoserva-generated smb.conf, mutual read/write between SMB and the plain filesystem as $TEST_UID:$TEST_GID)"
