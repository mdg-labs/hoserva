#!/usr/bin/env bash
# Tests for tools/unraid/prepare-migration.sh against hand-written fixture
# roots (testdata/root) with stand-ins for docker, findmnt, lsblk, df and
# zip on PATH. Nothing here touches a real Unraid server, a real block
# device or a real mount (D20): the script's HOSERVA_UNRAID_ROOT points at a
# throwaway copy of the fixture tree under a mktemp directory.
#
# The safety test comes first: a whole-tree snapshot before and after the
# run must be identical except for /boot/config/hoserva/, and control arms
# that change one thing elsewhere must make that comparison fail.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/prepare-migration.sh"
data="$here/testdata"
stubs="$data/stubs"

for tool in jq python3 unzip sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "test-prepare-migration: $tool is required to run these tests" >&2
    exit 1
  }
done

tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT

fail=0
cases=0
note() { printf 'test-prepare-migration: %s\n' "$*" >&2; }
ok() { cases=$((cases + 1)); }
bad() {
  fail=1
  cases=$((cases + 1))
  note "FAIL: $*"
}

assert_eq() { # got want what
  if [ "$1" = "$2" ]; then ok; else bad "$3: got '$1', want '$2'"; fi
}
assert_has() { # file fixed-string what
  if grep -qF -- "$2" "$1"; then ok; else bad "$3: '$2' not found in $1"; fi
}
assert_lacks() { # file fixed-string what
  if grep -qF -- "$2" "$1"; then bad "$3: '$2' unexpectedly found in $1"; else ok; fi
}
assert_file() { if [ -f "$1" ]; then ok; else bad "$2: $1 does not exist"; fi; }
assert_nofile() { if [ -e "$1" ] || [ -L "$1" ]; then bad "$2: $1 exists"; else ok; fi; }

n=0
R=""
OUT=""
STATUS=0

# new_root: a fresh copy of the fixture tree, an empty array of two disks,
# and an empty cache. Sets R (the root) and OUT (where results go).
new_root() {
  n=$((n + 1))
  OUT="$tmp/case$n"
  R="$OUT/root"
  mkdir -p "$OUT"
  cp -R -- "$data/root" "$R"
  mkdir -p "$R/mnt/disk1" "$R/mnt/disk2" "$R/mnt/cache/appdata" "$R/mnt/cache/domains" "$R/mnt/cache/system"
  printf 'disk1 1000000000\ndisk2 1000000000\n' >"$OUT/df.txt"
  : >"$OUT/violations"
}

mkdata() { # path KiB
  mkdir -p "$(dirname "$1")"
  head -c "$(($2 * 1024))" /dev/zero >"$1"
}

# run [args...]: runs the script as the stubs expect; stdout to $OUT/stdout,
# stderr to $OUT/stderr, exit status in STATUS. The stand-ins that must
# never be called log to $OUT/violations, checked after every run.
run() {
  STATUS=0
  env PATH="$stubs:$PATH" \
    HOSERVA_UNRAID_ROOT="$R" \
    STUB_VIOLATIONS="$OUT/violations" \
    STUB_DOCKER_DATA="$data/docker" \
    STUB_DF="$OUT/df.txt" \
    STUB_LSBLK="${STUB_LSBLK:-$data/lsblk-usb.txt}" \
    STUB_ZIP_LOG="$OUT/zip.log" \
    bash "$script" "$@" >"$OUT/stdout" 2>"$OUT/stderr" </dev/null || STATUS=$?
  if [ -s "$OUT/violations" ]; then
    bad "case $n: a forbidden command was called: $(tr '\n' ';' <"$OUT/violations")"
  else
    ok
  fi
}

report() { printf '%s' "$R/boot/config/hoserva/report.txt"; }
cap() { printf '%s' "$R/boot/config/hoserva/$1"; }

# snapshot: every entry under the root except the capture directory, with
# type, mode, size, link target and content checksum.
snapshot() {
  (
    cd "$1"
    find . -path ./boot/config/hoserva -prune -o \( -type d -printf '%p|d|%m\n' \) -o -printf '%p|%y|%m|%s|%l\n' | LC_ALL=C sort
    find . -path ./boot/config/hoserva -prune -o -type f -print0 | LC_ALL=C sort -z | xargs -0 -r sha256sum
  )
}

seed_cache_data() {
  mkdata "$R/mnt/cache/appdata/plex/db" 1024
  mkdata "$R/mnt/cache/appdata/sonarr/db" 512
  mkdata "$R/mnt/cache/domains/vm1/vdisk1.img" 2048
  mkdata "$R/mnt/cache/system/libvirt/libvirt.img" 1024
  mkdata "$R/mnt/cache/system/docker/dockerdir/layer" 8192
}

# --------------------------------------------------------------------
# 1. Nothing outside /boot/config/hoserva/ changes (the data-loss case).
note "case: nothing outside the capture directory changes"
new_root
seed_cache_data
mkdata "$R/mnt/disk1/media/film.mkv" 256
snapshot "$R" >"$OUT/before"
run
snapshot "$R" >"$OUT/after"
assert_eq "$STATUS" 0 "safety: exit status"
assert_file "$(cap capture.json)" "safety: the run captured something"
if [ "$(wc -l <"$OUT/before")" -gt 40 ]; then ok; else bad "safety: the snapshot is too small to mean anything"; fi
if diff -u "$OUT/before" "$OUT/after" >"$OUT/diff"; then ok; else bad "safety: the tree changed: $(head -20 "$OUT/diff")"; fi
run
snapshot "$R" >"$OUT/after2"
if diff -q "$OUT/before" "$OUT/after2" >/dev/null; then ok; else bad "safety: a second run changed the tree"; fi

for arm in append create delete mode; do
  note "control arm: $arm"
  new_root
  seed_cache_data
  snapshot "$R" >"$OUT/before"
  STUB_MUTATE=$arm run
  snapshot "$R" >"$OUT/after"
  if diff -q "$OUT/before" "$OUT/after" >/dev/null; then
    bad "control arm '$arm' went unnoticed: the snapshot comparison cannot detect a stray write"
  else
    ok
  fi
done

# --------------------------------------------------------------------
note "case: the capture files"
new_root
seed_cache_data
run
assert_eq "$STATUS" 0 "capture: exit status"
assert_eq "$(jq -S . "$(cap containers.json)")" "$(jq -S . "$data/docker/containers.json")" "containers.json is docker's own inspect output"
assert_eq "$(jq -S . "$(cap networks.json)")" "$(jq -S . "$data/docker/networks.json")" "networks.json is docker's own inspect output"
if cmp -s "$(cap disks.ini)" "$R/var/local/emhttp/disks.ini"; then ok; else bad "disks.ini is not a copy"; fi
if cmp -s "$(cap autostart)" "$R/var/lib/docker/unraid-autostart"; then ok; else bad "autostart is not a copy"; fi
if cmp -s "$(cap smart/disk1)" "$R/var/local/emhttp/smart/disk1"; then ok; else bad "smart/disk1 is not a copy"; fi
if cmp -s "$(cap smart/disk2)" "$R/var/local/emhttp/smart/disk2"; then ok; else bad "smart/disk2 is not a copy"; fi
assert_eq "$(cat "$(cap var.ini)")" 'sbSynced="1790000000"
sbSyncExit="0"
sbSyncErrs="0"
mdResync="0"' "var.ini holds the parity-check fields and no other key"
assert_lacks "$(cap var.ini)" "regKey" "var.ini leaks no registration key"
assert_eq "$(jq -r .unraid_version "$(cap capture.json)")" 7.3.2 "capture.json unraid_version"
assert_eq "$(jq -r .script_version "$(cap capture.json)")" unreleased "capture.json script_version"
assert_eq "$(jq -r .boot.mode "$(cap capture.json)")" usb "capture.json boot.mode"
assert_eq "$(jq -r .boot.filesystem "$(cap capture.json)")" vfat "capture.json boot.filesystem"
assert_eq "$(jq -r '.boot.devices[0].serial' "$(cap capture.json)")" FIXTURE-USB-SERIAL "capture.json boot device serial"
assert_eq "$(jq -r '.boot.devices[0].model' "$(cap capture.json)")" "Fixture USB Stick" "capture.json boot device model"
assert_eq "$(jq -r '.boot.mirrored' "$(cap capture.json)")" false "capture.json boot.mirrored"
assert_eq "$(jq -r '.boot.shared_with_data_pool' "$(cap capture.json)")" false "capture.json boot.shared_with_data_pool"
assert_eq "$(jq -r .docker.state "$(cap capture.json)")" running "capture.json docker.state"
assert_eq "$(jq -r .docker.directory_location "$(cap capture.json)")" cache "capture.json docker directory location"
assert_eq "$(jq -r .libvirt_img_location "$(cap capture.json)")" cache "capture.json libvirt.img location"
assert_eq "$(jq -r '.docker.writable_layers[] | select(.container == "plex") | .bytes' "$(cap capture.json)")" 1500000 "writable layer of plex in bytes"
assert_eq "$(jq -r '.docker.writable_layers[] | select(.container == "Radarr") | .bytes' "$(cap capture.json)")" 12300 "writable layer of Radarr in bytes"
assert_eq "$(jq -r '.docker.writable_layers[] | select(.container == "stack-web") | .bytes' "$(cap capture.json)")" 2250000000 "writable layer of stack-web in bytes"
if jq -e '.captured_at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")' "$(cap capture.json)" >/dev/null; then ok; else bad "captured_at is not an RFC 3339 UTC time"; fi
assert_has "$(cap containers.json)" "FIXTURE-SECRET-VALUE" "the capture holds the environment, secrets included"
for f in "$OUT/stderr" "$(report)"; do
  assert_lacks "$f" "FIXTURE-SECRET-VALUE" "no environment value in the report"
  assert_lacks "$f" "FIXTURE-API-KEY-VALUE" "no environment value in the report"
  assert_lacks "$f" "FIXTURE-DB-PASSWORD" "no environment value in the report"
  assert_lacks "$f" "FIXTURE-NOT-A-REAL-KEY" "no registration key in the report"
done
assert_has "$(report)" "environment variables, secrets included" "the report states that the capture holds secrets"
if cmp -s "$OUT/stderr" "$(report)"; then ok; else bad "stderr and report.txt differ"; fi
assert_eq "$(wc -c <"$OUT/stdout")" 0 "no zip, nothing on stdout"
assert_eq "$(find "$R/boot/config/hoserva" -name '.*' | wc -l)" 0 "no temporary file is left in the capture directory"

# --------------------------------------------------------------------
note "case: no containers"
new_root
STUB_CONTAINERS="$data/docker/empty.json" run
assert_eq "$STATUS" 0 "no containers: exit status"
assert_eq "$(cat "$(cap containers.json)")" '[]' "no containers: containers.json is []"
assert_eq "$(jq -r .docker.state "$(cap capture.json)")" running "no containers: docker state"
assert_eq "$(jq -c .docker.writable_layers "$(cap capture.json)")" '[]' "no containers: no writable layers"
assert_has "$(report)" "0 containers" "no containers: the report says so"

# --------------------------------------------------------------------
note "case: Docker stopped"
new_root
mkdir -p "$R/boot/config/hoserva/smart"
printf 'stale' >"$R/boot/config/hoserva/containers.json"
printf 'stale' >"$R/boot/config/hoserva/networks.json"
printf 'stale' >"$R/boot/config/hoserva/autostart"
printf 'stale' >"$R/boot/config/hoserva/smart/disk9"
rm -f "$R/var/lib/docker/unraid-autostart"
STUB_DOCKER_MODE=stopped run
assert_eq "$STATUS" 0 "docker stopped: exit status"
assert_nofile "$(cap containers.json)" "docker stopped: containers.json is absent, not empty"
assert_nofile "$(cap networks.json)" "docker stopped: networks.json is absent, not empty"
assert_nofile "$(cap autostart)" "no autostart list: a stale copy from an earlier run is removed"
assert_nofile "$(cap smart/disk9)" "a stale smart file from an earlier run is removed"
assert_file "$(cap smart/disk1)" "smart/ is captured"
assert_eq "$(jq -r .docker.state "$(cap capture.json)")" stopped "docker stopped: the capture says so"
assert_eq "$(jq -c .docker.writable_layers "$(cap capture.json)")" null "docker stopped: no writable-layer data"
assert_has "$(report)" "!! Docker is not running" "docker stopped: the report says so"
assert_has "$(report)" "No autostart list" "no autostart list: the report says so"

note "case: Docker fails after answering ps"
new_root
STUB_DOCKER_MODE=inspect-fails run
assert_eq "$STATUS" 0 "docker error: exit status"
assert_nofile "$(cap containers.json)" "docker error: containers.json is absent"
assert_nofile "$(cap networks.json)" "docker error: networks.json is absent"
assert_eq "$(jq -r .docker.state "$(cap capture.json)")" error "docker error: the capture says so"
assert_has "$(report)" "!! Docker did not answer completely" "docker error: the report says so"

# --------------------------------------------------------------------
note "case: containers by origin, templates matched on <Name>"
new_root
run
assert_has "$(report)" "6 containers: 4 running, 2 not running." "container counts"
assert_has "$(report)" "4 from the Docker page (dockerMan templates)" "origin counts"
assert_has "$(report)" "1 from Compose Manager" "origin counts"
assert_has "$(report)" "1 created by hand" "origin counts"
assert_has "$(report)" "- plex  [running]  dockerMan" "plex row"
assert_has "$(report)" "- stack-web  [running]  Compose Manager project media-stack" "compose row"
assert_has "$(report)" "- handmade  [exited]  created by hand" "manual row"
assert_has "$(report)" "!! 2 dockerMan container(s) have no template whose <Name> matches: Radarr orphan" "containers without a template"
assert_lacks "$(report)" "matches: plex" "plex has its template (the file name differs from <Name>)"
assert_lacks "$(report)" "never-installed" "an unused template is not a container"
assert_has "$(report)" "Created by hand (no template to convert; recreate them from their run command): handmade" "manual list"
assert_lacks "$(report)" "matches: Radarr orphan stack-web" "a Compose Manager container needs no template"

note "case: every dockerMan container has a template"
new_root
printf '<Container version="2"><Name>Radarr</Name></Container>\n' >"$R/boot/config/plugins/dockerMan/templates-user/r2.xml"
printf '<Container version="2">\n  <Name>\n orphan </Name></Container>\n' >"$R/boot/config/plugins/dockerMan/templates-user/o2.xml"
run
assert_lacks "$(report)" "no template whose" "all matched, multi-line and padded <Name> too"

note "case: no template directory at all"
new_root
rm -rf "$R/boot/config/plugins/dockerMan"
run
assert_has "$(report)" "!! 4 dockerMan container(s) have no template whose <Name> matches: plex sonarr Radarr orphan" "a missing template directory matches nothing"

# --------------------------------------------------------------------
note "case: autostart order and waits"
new_root
run
assert_has "$(report)" "1. plex (waits 30 s before the next)" "autostart wait"
assert_has "$(report)" "2. sonarr" "autostart order"
assert_has "$(report)" "3. radarr (waits 10 s before the next)" "autostart blank lines are skipped"

# --------------------------------------------------------------------
note "case: data still on the cache"
new_root
seed_cache_data
run
assert_eq "$STATUS" 0 "cache data: exit status"
assert_has "$(report)" "!! Data is still on the cache (3 directories)" "cache data: hard warning"
assert_has "$(report)" 'appdata on pool cache: 1.5 MiB; share cache setting "only"; the appdata directory is on the cache' "cache data: appdata row"
assert_has "$(report)" 'domains on pool cache: 2.0 MiB; share cache setting "prefer"; the domains directory is on the cache' "cache data: domains row"
assert_has "$(report)" 'system on pool cache: 1.0 MiB; share cache setting "prefer"; the system directory is on the cache' "cache data: system row excludes Docker's directory"
assert_has "$(report)" "!! libvirt.img (/mnt/user/system/libvirt/libvirt.img) is on the cache" "cache data: libvirt.img"
assert_has "$(report)" "Docker storage (/mnt/user/system/docker/dockerdir) is on the cache. It is not moved" "Docker directory reported separately"
assert_has "$(report)" "- plex: 1.4 MiB" "writable layer listed per container"
assert_has "$(report)" "- stack-web: 2.0 GiB" "writable layer listed per container (large)"
assert_has "$(report)" "Fit: the data on the cache (4.5 MiB) fits on the array" "cache data: fits"

note "case: a cache that holds only Docker's own storage"
new_root
mkdata "$R/mnt/cache/system/docker/dockerdir/layer" 8192
run
assert_lacks "$(report)" "!! Data is still on the cache" "docker directory alone is not data to move"
assert_has "$(report)" "Nothing is left on the cache to move." "docker directory alone"
assert_has "$(report)" "Docker storage (/mnt/user/system/docker/dockerdir) is on the cache." "docker directory still reported"
assert_lacks "$(report)" "does NOT fit" "docker directory alone: no fit warning"
assert_eq "$(grep -c '^DOCKER_IMAGE_FILE=".*/"$' "$R/boot/config/docker.cfg")" 1 "the fixture keeps the directory in DOCKER_IMAGE_FILE with a trailing slash"

note "case: an empty cache"
new_root
mkdir -p "$R/mnt/cache/appdata/plex"
run
assert_lacks "$(report)" "!! Data is still on the cache" "empty directories are not data"
assert_has "$(report)" "Nothing is left on the cache to move." "empty cache"

note "case: other data on the cache"
new_root
mkdata "$R/mnt/cache/media/film.mkv" 1024
mkdata "$R/mnt/cache/stray/file" 1024
mkdir -p "$R/mnt/cache/lost+found"
mkdata "$R/mnt/cache/lost+found/x" 1024
run
assert_has "$(report)" '!! Data is still on the cache (2 directories)' "other data counted, lost+found skipped"
assert_has "$(report)" 'media on pool cache: 1.0 MiB; share cache setting "yes" (the mover has not moved it)' "a share set to yes"
assert_has "$(report)" 'stray on pool cache: 1.0 MiB; no share configuration' "a plain directory"

# --------------------------------------------------------------------
note "case: a cache that does not fit"
new_root
seed_cache_data
printf 'disk1 2048\ndisk2 1024\n' >"$OUT/df.txt"
run
assert_has "$(report)" "!! The data on the cache (4.5 MiB) does NOT fit on the array: 3.0 MiB usable of 3.0 MiB free across 2 data disks" "does not fit"
assert_lacks "$(report)" "Fit: the data on the cache" "does not fit: no fit line"

note "case: share floors are kept free on each disk"
new_root
seed_cache_data
printf 'shareUseCache="only"\nshareFloor="1000"\n' >"$R/boot/config/shares/appdata.cfg"
printf 'disk1 3000\ndisk2 3000\n' >"$OUT/df.txt"
run
assert_has "$(report)" "!! The data on the cache (4.5 MiB) does NOT fit on the array: 3.9 MiB usable of 5.8 MiB free across 2 data disks, after the largest share floor (1000 KiB) is kept free on each." "floors reduce usable space"
new_root
seed_cache_data
printf 'shareUseCache="only"\nshareFloor="1000"\n' >"$R/boot/config/shares/appdata.cfg"
printf 'disk1 500\ndisk2 3000\n' >"$OUT/df.txt"
run
assert_has "$(report)" "1.9 MiB usable of 3.4 MiB free" "a disk below the floor contributes nothing, not a negative number"

note "case: free space cannot be read"
new_root
seed_cache_data
printf 'disk1 1000000\n' >"$OUT/df.txt"
run
assert_has "$(report)" "could not read the free space of disk2" "df failure is reported"
assert_has "$(report)" "!! The free space of the array could not be read" "unknown free space is a warning, not a fit"
assert_lacks "$(report)" "Fit: the data on the cache" "unknown free space is not reported as fitting"

# --------------------------------------------------------------------
note "case: last parity check"
new_root
run
assert_has "$(report)" "Last parity check: 2026-09-21 14:13 UTC, exit code 0, 0 errors." "clean parity check"
assert_lacks "$(report)" "parity check did not finish" "clean parity check raises nothing"

new_root
sed -i 's/sbSyncExit="0"/sbSyncExit="-4"/' "$R/var/local/emhttp/var.ini"
run
assert_has "$(report)" "!! The last parity check did not finish clean (exit code -4)" "failed parity check"

new_root
sed -i 's/sbSyncErrs="0"/sbSyncErrs="12"/' "$R/var/local/emhttp/var.ini"
run
assert_has "$(report)" "Last parity check: 2026-09-21 14:13 UTC, exit code 0, 12 errors." "parity errors"
assert_has "$(report)" "!! The last parity check found 12 errors." "parity errors warning"

new_root
sed -i 's/mdResync="0"/mdResync="123456"/' "$R/var/local/emhttp/var.ini"
run
assert_has "$(report)" "!! A parity check or sync is running right now." "running parity check"

new_root
sed -i 's/sbSynced="1790000000"/sbSynced="0"/' "$R/var/local/emhttp/var.ini"
run
assert_has "$(report)" "!! No parity check has completed on this array." "no parity check yet"

new_root
rm -f "$R/var/local/emhttp/var.ini"
run
assert_has "$(report)" "!! The parity-check result could not be read from var.ini." "no var.ini"
assert_nofile "$(cap var.ini)" "no var.ini: nothing captured"

# --------------------------------------------------------------------
note "case: serial to slot table"
new_root
run
assert_has "$(report)" "- parity (Parity): FIXTURE_MODEL_PARITY0001 as sdb, DISK_OK" "parity row"
assert_has "$(report)" "- disk1 (Data): FIXTURE_MODEL_DATA0001 as sdc, DISK_OK" "data row"
assert_has "$(report)" "- disk3 (Data): (empty slot), DISK_NP" "empty slot"
assert_has "$(report)" "Parity disk(s): parity." "parity disk named"

new_root
rm -f "$R/var/local/emhttp/disks.ini"
run
assert_has "$(report)" "!! disks.ini was not found" "no disks.ini"
assert_nofile "$(cap disks.ini)" "no disks.ini: nothing captured"

# --------------------------------------------------------------------
note "case: boot modes and rollback"
new_root
run
assert_has "$(report)" "Unraid boots from a USB stick (vfat)." "USB boot"
assert_has "$(report)" "Debian on a separate device: reinsert the stick and boot." "USB rollback row 1"
assert_has "$(report)" "Debian on the shared NVMe (with the Unraid cache): reinsert the stick, re-create the Unraid cache, and move appdata back." "USB rollback row 2"

new_root
STUB_FSTYPE=zfs STUB_SOURCE=fixturepool/boot STUB_LSBLK="$data/lsblk-zfs-single.txt" run
assert_eq "$(jq -r .boot.mode "$(cap capture.json)")" internal "internal boot: mode"
assert_eq "$(jq -r .boot.filesystem "$(cap capture.json)")" zfs "internal boot: filesystem"
assert_eq "$(jq -r '.boot.devices | length' "$(cap capture.json)")" 1 "internal boot: one device"
assert_eq "$(jq -r '.boot.devices[0].serial' "$(cap capture.json)")" FIXTURE-NVME-A "internal boot: device serial"
assert_eq "$(jq -r .boot.mirrored "$(cap capture.json)")" false "internal boot: not mirrored"
assert_eq "$(jq -r .boot.shared_with_data_pool "$(cap capture.json)")" false "internal boot: not shared"
assert_has "$(report)" "Unraid boots from an internal boot pool (zfs), 1 device(s)." "internal boot"
assert_has "$(report)" "Debian on another device: switch the firmware boot order back." "internal dedicated rollback 1"
assert_has "$(report)" "Debian on this boot device: restore the Flash Backup zip to a USB stick (USB Flash Creator) and boot it." "internal dedicated rollback 2"
assert_lacks "$(report)" "reinsert the stick" "internal boot has no stick"

new_root
STUB_FSTYPE=zfs STUB_SOURCE=fixturepool/boot STUB_LSBLK="$data/lsblk-zfs-mirror.txt" run
assert_eq "$(jq -r .boot.mirrored "$(cap capture.json)")" true "mirrored boot pool"
assert_eq "$(jq -r '.boot.devices | length' "$(cap capture.json)")" 2 "mirrored boot pool: two devices"
assert_has "$(report)" "2 device(s), mirrored." "mirrored boot pool in the report"

new_root
printf 'diskId="FIXTURE_CACHE_SSD_FX0001"\ndiskBootSize="16000"\n' >"$R/boot/config/pools/cache.cfg"
STUB_FSTYPE=zfs STUB_SOURCE=fixturepool/boot STUB_LSBLK="$data/lsblk-zfs-single.txt" run
assert_eq "$(jq -r .boot.shared_with_data_pool "$(cap capture.json)")" true "boot pool shared with a data pool (pool config)"
assert_has "$(report)" "Debian on the same NVMe: restore the Flash Backup zip to a USB stick (USB Flash Creator) and boot it; re-create the cache." "internal shared rollback"
assert_lacks "$(report)" "switch the firmware boot order back" "internal shared has one row"

new_root
STUB_FSTYPE=zfs STUB_SOURCE=fixturepool/boot STUB_LSBLK="$data/lsblk-zfs-shared.txt" run
assert_eq "$(jq -r .boot.shared_with_data_pool "$(cap capture.json)")" true "boot pool shared with a data pool (partition on the boot device)"

new_root
STUB_FSTYPE=ext4 STUB_SOURCE=/dev/sda1 run
assert_eq "$(jq -r .boot.mode "$(cap capture.json)")" unknown "unknown boot mode"
assert_has "$(report)" "!! The boot mode could not be determined" "unknown boot mode is a warning"

new_root
STUB_FSTYPE=none run
assert_eq "$(jq -r .boot.mode "$(cap capture.json)")" unknown "no mount for /boot"
assert_has "$(report)" "!! The boot mode could not be determined (/boot is 'not a mount point')" "no mount for /boot is a warning"

# --------------------------------------------------------------------
note "case: the Docker directory and libvirt.img on the array and the boot pool"
new_root
mkdata "$R/mnt/disk1/system/docker/dockerdir/layer" 64
mkdata "$R/mnt/disk2/system/libvirt/libvirt.img" 64
run
assert_eq "$(jq -r .docker.directory_location "$(cap capture.json)")" array "docker directory on the array"
assert_eq "$(jq -r .libvirt_img_location "$(cap capture.json)")" array "libvirt.img on the array"
assert_lacks "$(report)" "!! libvirt.img" "libvirt.img on the array is fine"

new_root
rm -f "$R/boot/config/docker.cfg" "$R/boot/config/domain.cfg"
run
assert_eq "$(jq -r .docker.directory_location "$(cap capture.json)")" none "docker not configured"
assert_eq "$(jq -r .libvirt_img_location "$(cap capture.json)")" none "no VM manager"

new_root
printf 'diskId="x"\ndiskBootSize="16000"\n' >"$R/boot/config/pools/cache.cfg"
mkdata "$R/mnt/cache/system/libvirt/libvirt.img" 64
run
assert_eq "$(jq -r .libvirt_img_location "$(cap capture.json)")" boot-pool "libvirt.img on a pool that shares the boot device"

new_root
printf 'DOCKER_IMAGE_TYPE="btrfs"\nDOCKER_IMAGE_FILE="/mnt/user/system/docker/docker.img"\n' >"$R/boot/config/docker.cfg"
mkdata "$R/mnt/cache/system/docker/docker.img" 4096
run
assert_eq "$(jq -r .docker.directory_location "$(cap capture.json)")" cache "docker.img on the cache (image mode)"
assert_has "$(report)" "Nothing is left on the cache to move." "docker.img in image mode is not data to move"

# --------------------------------------------------------------------
note "case: --zip"
new_root
seed_cache_data
run --zip
assert_eq "$STATUS" 0 "zip: exit status"
assert_eq "$(head -c 2 "$OUT/stdout")" PK "zip: stdout is a zip"
unzip -Z1 "$OUT/stdout" | LC_ALL=C sort >"$OUT/entries"
assert_has "$OUT/entries" "config/hoserva/capture.json" "zip: the capture is inside (it is written first)"
assert_has "$OUT/entries" "config/hoserva/report.txt" "zip: the report is inside"
assert_has "$OUT/entries" "config/hoserva/containers.json" "zip: containers.json is inside"
assert_has "$OUT/entries" "config/shares/appdata.cfg" "zip: config/ is a path inside the zip"
assert_has "$OUT/entries" "bzimage" "zip: the root is /boot"
assert_has "$OUT/entries" "EFI/boot/bootx64.efi" "zip: nested entries"
assert_lacks "$OUT/entries" "previous/" "zip: previous/ is left out"
assert_lacks "$OUT/entries" "prev/" "zip: prev/ is left out"
assert_lacks "$OUT/entries" "mnt/" "zip: nothing outside /boot"
assert_has "$OUT/zip.log" "cwd=$R/boot" "zip: it runs in /boot"
assert_has "$(report)" "Hoserva Phase A report" "zip: the report still goes to stderr"
assert_eq "$(unzip -p "$OUT/stdout" config/hoserva/capture.json | jq -r .unraid_version)" 7.3.2 "zip: the captured file is the one just written"

if command -v zip >/dev/null 2>&1; then
  new_root
  mkdir -p "$OUT/realbin"
  for t in docker findmnt lsblk df emcmd mdcmd efibootmgr mover; do cp "$stubs/$t" "$OUT/realbin/$t"; done
  STATUS=0
  env PATH="$OUT/realbin:$PATH" HOSERVA_UNRAID_ROOT="$R" STUB_VIOLATIONS="$OUT/violations" STUB_DOCKER_DATA="$data/docker" \
    STUB_DF="$OUT/df.txt" STUB_LSBLK="$data/lsblk-usb.txt" bash "$script" --zip >"$OUT/stdout" 2>"$OUT/stderr" </dev/null || STATUS=$?
  assert_eq "$STATUS" 0 "real zip: exit status"
  unzip -Z1 "$OUT/stdout" | LC_ALL=C sort >"$OUT/entries"
  assert_has "$OUT/entries" "config/hoserva/capture.json" "real zip: the capture is inside"
  assert_has "$OUT/entries" "bzimage" "real zip: the root is /boot"
  assert_lacks "$OUT/entries" "previous/" "real zip: previous/ is left out"
  assert_lacks "$OUT/entries" "prev/" "real zip: prev/ is left out"
else
  note "zip is not installed here: the real-zip case was skipped (the stand-in case above ran)"
fi

note "case: zip fails after the capture"
new_root
STUB_ZIP_FAIL=1 run --zip
if [ "$STATUS" -ne 0 ]; then ok; else bad "a failed zip exited 0"; fi
assert_has "$OUT/stderr" "zip failed: the stream is incomplete" "a failed zip says the stream is unusable"
assert_file "$(cap capture.json)" "a failed zip still leaves the capture"

note "case: the flash cannot be written"
if [ "$(id -u)" = 0 ]; then
  note "running as root: the read-only flash case was skipped"
else
  new_root
  chmod a-w "$R/boot/config"
  run --zip
  chmod u+w "$R/boot/config"
  if [ "$STATUS" -ne 0 ]; then ok; else bad "an unwritable flash exited 0"; fi
  assert_eq "$(wc -c <"$OUT/stdout")" 0 "an unwritable flash: no zip is streamed without the capture"
  assert_has "$OUT/stderr" "cannot create" "an unwritable flash says why"
  assert_nofile "$R/boot/config/hoserva" "an unwritable flash: nothing written"
fi

note "case: --zip refused on a terminal"
new_root
STATUS=0
env PATH="$stubs:$PATH" HOSERVA_UNRAID_ROOT="$R" STUB_VIOLATIONS="$OUT/violations" STUB_DOCKER_DATA="$data/docker" \
  STUB_DF="$OUT/df.txt" STUB_LSBLK="$data/lsblk-usb.txt" STUB_ZIP_LOG="$OUT/zip.log" \
  python3 -c '
import os, pty, sys
status = pty.spawn(["bash", sys.argv[1], "--zip"])
sys.exit(os.waitstatus_to_exitcode(status))
' "$script" >"$OUT/tty-output" 2>&1 </dev/null || STATUS=$?
if [ "$STATUS" -ne 0 ]; then ok; else bad "--zip on a terminal exited 0"; fi
assert_has "$OUT/tty-output" "stdout is a terminal" "--zip on a terminal says why"
assert_nofile "$R/boot/config/hoserva" "--zip on a terminal wrote nothing"
assert_nofile "$OUT/zip.log" "--zip on a terminal ran no zip"

note "case: --zip entry that starts with a dash"
new_root
printf 'x' >"$R/boot/-weird"
run --zip
if [ "$STATUS" -ne 0 ]; then ok; else bad "a dash entry was zipped silently"; fi
assert_has "$OUT/stderr" "starts with '-'" "a dash entry is refused with a reason"

# --------------------------------------------------------------------
note "case: arguments, platform checks, piping"
new_root
run --bogus
assert_eq "$STATUS" 2 "unknown argument: exit status"
assert_nofile "$R/boot/config/hoserva" "unknown argument: nothing written"

new_root
rm -f "$R/etc/unraid-version"
run
if [ "$STATUS" -ne 0 ]; then ok; else bad "a system without /etc/unraid-version was accepted"; fi
assert_has "$OUT/stderr" "does not look like an Unraid server" "not Unraid: the reason"
assert_nofile "$R/boot/config/hoserva" "not Unraid: nothing written"

new_root
rm -rf "$R/boot/config"
run
if [ "$STATUS" -ne 0 ]; then ok; else bad "a missing /boot/config was accepted"; fi
assert_nofile "$R/boot/config" "no /boot/config: nothing created"

new_root
STATUS=0
env PATH="$stubs:$PATH" HOSERVA_UNRAID_ROOT="$R" STUB_VIOLATIONS="$OUT/violations" STUB_DOCKER_DATA="$data/docker" \
  STUB_DF="$OUT/df.txt" STUB_LSBLK="$data/lsblk-usb.txt" STUB_ZIP_LOG="$OUT/zip.log" \
  bash -s -- --zip <"$script" >"$OUT/stdout" 2>"$OUT/stderr" || STATUS=$?
assert_eq "$STATUS" 0 "piped into bash -s: exit status"
assert_eq "$(head -c 2 "$OUT/stdout")" PK "piped into bash -s: the zip is produced"
assert_file "$(cap capture.json)" "piped into bash -s: captured"
assert_eq "$(wc -c <"$OUT/violations")" 0 "piped into bash -s: no forbidden command"

new_root
head -c 600 "$script" >"$OUT/truncated.sh"
STATUS=0
env PATH="$stubs:$PATH" HOSERVA_UNRAID_ROOT="$R" bash "$OUT/truncated.sh" >/dev/null 2>&1 || STATUS=$?
assert_nofile "$R/boot/config/hoserva" "a truncated download does nothing"

# --------------------------------------------------------------------
if [ "$fail" -ne 0 ]; then
  note "FAILED"
  exit 1
fi
note "ok ($cases checks)"
