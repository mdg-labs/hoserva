#!/usr/bin/env bash
# Runs inside the L3 guest as root, started by unraid-capture.sh (issue #74):
# the guest-side half of `make vm-unraid-capture`.
#
# It rebuilds the layout of a running Unraid server under a scratch root
# (HOSERVA_UNRAID_ROOT of tools/unraid/prepare-migration.sh): the fixture's
# flash on a FAT32 image mounted at <root>/boot, the fixture's disks mounted
# read-only at <root>/mnt/<slot>, and Unraid's runtime state under
# <root>/var/local/emhttp. It starts the fixture's containers under the guest's
# Docker (images are imported from a busybox root filesystem, so nothing is
# pulled from a registry), runs the prepare script against that root, and
# leaves what it wrote under /boot/config/hoserva/ in <output>/capture/.
#
# The containers cover: dockerMan containers with a template, one stopped, one
# on a custom ipvlan network, one dockerMan container whose template does not
# exist, one created by hand and one from a Compose Manager project.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
GUEST_DIR=$(cd -- "$HERE/../.." && pwd)
PREPARE="$GUEST_DIR/tools/unraid/prepare-migration.sh"
# shellcheck source=scripts/devenv/unraid-fixture.sh
source "$GUEST_DIR/scripts/devenv/unraid-fixture.sh"

TIER=l3
VARIANT=${1:-}
[[ -n $VARIANT ]] || die "usage: unraid-capture-guest.sh <variant>"
init_tier
parse_spec "$VDIR/spec"
[[ -d $FLASH/config ]] || die "$FLASH has no flash tree: build $VARIANT first"

CAP="$OUT/capture-root"
RESULT="$OUT/capture"
BOOT_IMG="$OUT/boot.img"
bootdev=""
containers_owned=false

# shellcheck disable=SC2317 # called from the EXIT trap
cleanup_capture() {
  local rc=$?
  set +e
  if [[ $containers_owned == true ]]; then
    docker rm -f notes mediaserver photos syncer gateway dbtool handmade stack-web >/dev/null 2>&1
    docker network rm br0 stack_default >/dev/null 2>&1
  fi
  cleanup
  if [[ -n $bootdev ]]; then
    if [[ $(losetup --noheadings --output BACK-FILE "$bootdev" 2>/dev/null) == "$BOOT_IMG" ]]; then losetup -d "$bootdev"; fi
  fi
  return $rc
}
trap cleanup_capture EXIT

if ! command -v docker >/dev/null 2>&1 || ! command -v docker-compose >/dev/null 2>&1 || [[ ! -x /bin/busybox ]]; then
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends docker.io docker-cli docker-compose busybox-static >/dev/null
fi
command -v mkfs.vfat >/dev/null 2>&1 || die "mkfs.vfat is not installed"
systemctl start docker
docker info >/dev/null || die "the Docker daemon does not answer"

rm -rf --one-file-system -- "$CAP" "$RESULT"
mkdir -p -- "$CAP/mnt" "$CAP/boot" "$CAP/var/local/emhttp/smart" "$CAP/var/lib/docker" "$CAP/etc" "$RESULT"
MNT="$CAP/mnt"

# ---- the fixture's disks, read-only, where Unraid mounts them
for s in "${SLOTS[@]}"; do
  if [[ ${D_KIND[$s]} == parity ]]; then continue; fi
  existing_disk "$s"
  mount_fs "$s" ro
done

# ---- the flash, on a FAT32 image like the stick
rm -f -- "$BOOT_IMG"
truncate -s 128M "$BOOT_IMG"
mkfs.vfat -F 32 -n UNRAID "$BOOT_IMG" >/dev/null
bootdev=$(losetup --find --show "$BOOT_IMG")
mount -t vfat -o rw,noatime,umask=0,shortname=mixed "$bootdev" "$CAP/boot"
MOUNTED+=("$CAP/boot")
cp -r --no-preserve=mode,ownership,timestamps -- "$FLASH/." "$CAP/boot/"

# ---- Unraid's runtime state
# The serials the guest's disks report carry this lab's id; the committed
# capture must not.
sed -E 's/-hoserva-[^"]*/-capture/' "$FLASH/config/hoserva/disks.ini" >"$CAP/var/local/emhttp/disks.ini"
cp -- "$GUEST_DIR/testdata/unraid-fixtures/common/runtime/var.ini" "$CAP/var/local/emhttp/var.ini"
cp -- "$GUEST_DIR/testdata/unraid-fixtures/common/runtime/autostart" "$CAP/var/lib/docker/unraid-autostart"
for s in "${SLOTS[@]}"; do
  cp -- "$GUEST_DIR/testdata/unraid-fixtures/common/runtime/smart.txt" "$CAP/var/local/emhttp/smart/$s"
done
printf 'version="%s"\n' "$SPEC_VERSION" >"$CAP/etc/unraid-version"

# ---- the containers
work=$(mktemp -d "$OUT/capture-work.XXXXXX")
trap 'rm -rf -- "$work"; cleanup_capture' EXIT

existing_containers=$(docker ps -aq)
existing_networks=$(docker network ls --format '{{.Name}}' | awk '$0 != "bridge" && $0 != "host" && $0 != "none"')
[[ -z $existing_containers && -z $existing_networks ]] \
  || die "refusing to capture: this Docker daemon already has containers or networks of its own. Run the capture on a fresh guest ('make vm-up', or 'make vm-restore' to a snapshot taken before any container ran)"
containers_owned=true

mkdir -p -- "$work/rootfs/bin"
cp /bin/busybox "$work/rootfs/bin/busybox"
tar -C "$work/rootfs" -cf "$work/rootfs.tar" .
for img in notes:1.0 mediaserver:2.3 photos:latest syncer:1.0 gateway:0.9 web:1.0 dbtool:5 handmade:latest; do
  docker rmi -f "fixture/$img" >/dev/null 2>&1 || true
  docker import --change 'CMD ["/bin/busybox","sleep","31536000"]' "$work/rootfs.tar" "fixture/$img" >/dev/null
done

mkdir -p /mnt/user/appdata/{notes,mediaserver,photos,syncer,gateway,dbtool,handmade,stack} /mnt/user/documents /mnt/user/media/photos /mnt/user/backup /mnt/cache/transcode

dm() {  # name, docker run arguments..., image
  local name=$1
  shift
  docker run -d --name "$name" \
    --label net.unraid.docker.managed=dockerman \
    --label "net.unraid.docker.icon=https://example.invalid/fixture/$name.png" \
    -e TZ=UTC -e HOST_OS=Unraid -e HOST_HOSTNAME=fixture-tower -e "HOST_CONTAINERNAME=$name" \
    "$@" >/dev/null
}

parent=$(ip -o -4 route show default | awk '{print $5; exit}')
[[ -n $parent ]] || die "the guest has no default route to use as the ipvlan parent"
docker network create -d ipvlan --subnet 192.168.50.0/24 --gateway 192.168.50.1 -o parent="$parent" -o ipvlan_mode=l2 br0 >/dev/null

dm notes --label 'net.unraid.docker.webui=http://[IP]:[PORT:8080]/' \
  -e PUID=99 -e PGID=100 -e FIXTURE_API_TOKEN=fixture-not-a-secret \
  -p 8080:80/tcp -v /mnt/user/appdata/notes:/config:rw -v /mnt/user/documents:/data:ro fixture/notes:1.0
dm mediaserver --label 'net.unraid.docker.webui=http://[IP]:[PORT:9090]/' --network host \
  -v /mnt/user/appdata/mediaserver:/config:rw -v /mnt/user/media:/media:ro -v /mnt/cache/transcode:/transcode:rw fixture/mediaserver:2.3
dm photos --label 'net.unraid.docker.webui=http://[IP]:[PORT:8082]/' \
  -p 8082:8082/tcp -v /mnt/user/media/photos:/photos:rw -v /mnt/user/appdata/photos:/config:rw fixture/photos:latest
dm syncer -v /mnt/user/appdata/syncer:/config:rw -v /mnt/user/backup:/backup:rw fixture/syncer:1.0
docker stop -t 1 syncer >/dev/null
dm gateway --label 'net.unraid.docker.webui=http://[IP]:[PORT:8083]/' --network br0 --ip 192.168.50.20 --restart=unless-stopped \
  -v /mnt/user/appdata/gateway:/config:rw fixture/gateway:0.9
dm dbtool -v /mnt/user/appdata/dbtool:/data:rw fixture/dbtool:5
docker run -d --name handmade -p 9100:9100/tcp -v /mnt/user/appdata/handmade:/data:rw fixture/handmade:latest >/dev/null
(cd "$CAP/boot/config/plugins/compose.manager/projects/stack" && docker-compose -p stack up -d >/dev/null)

states=$(docker ps -a --format '{{.Names}}={{.State}}' | LC_ALL=C sort | tr '\n' ' ')
expected='dbtool=running gateway=running handmade=running mediaserver=running notes=running photos=running stack-web=running syncer=exited '
[[ $states == "$expected" ]] || die "the containers are not in the expected states: $states"

# ---- the prepare script
printf 'unraid-capture: running prepare-migration.sh against %s\n' "$CAP"
HOSERVA_UNRAID_ROOT="$CAP" bash "$PREPARE"
for f in containers.json networks.json capture.json report.txt var.ini autostart disks.ini; do
  [[ -s $CAP/boot/config/hoserva/$f ]] || die "the prepare script wrote no $f"
done
cp -r -- "$CAP/boot/config/hoserva/." "$RESULT/"
rm -f -- "$RESULT/disks.ini"
printf 'unraid-capture: captured %s under %s\n' "$VARIANT" "$RESULT"
