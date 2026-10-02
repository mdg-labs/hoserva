#!/usr/bin/env bash
# Hoserva Phase A prepare script. Run it as root on a still-running Unraid
# server, before the Flash Backup (doc 05 §4, Q89):
#
#   ssh root@<server> 'curl -fsSL <release-url>/prepare-migration.sh | bash -s -- --zip' > <server>-boot.zip
#
# It captures runtime state the migrator cannot get from the flash into
# /boot/config/hoserva/, prints the Phase A report on stderr (and saves it
# there as report.txt), and with --zip streams the flash as a zip on stdout.
#
# Read-only contract. The only writes are `mkdir -p /boot/config/hoserva`
# and, inside it, the capture files named in the report (written through a
# hidden temporary file and `mv`), plus removing those same files, and
# smart/*, left by an earlier run. Everything else is read: `docker ps`,
# `docker inspect`, `docker network ls`, `docker network inspect`,
# `findmnt`, `lsblk`, `df`, `du`, `find` (without -delete or -exec), `sed`,
# `date`, `cat` and, for --zip, `zip -qr -` to stdout. It never calls emcmd,
# mdcmd, a webGui script, efibootmgr, the mover or a plugin script, and it
# never writes under /mnt.
#
# Environment: HOSERVA_UNRAID_ROOT relocates /boot, /mnt, /var/local/emhttp,
# /var/lib/docker and /etc/unraid-version under another directory, so the
# script can be tested against a fixture tree without an Unraid server.
#
# The whole program is one function that is called on the last line: when
# the script arrives through a pipe, a truncated download defines the
# function and never runs it, and no command can swallow the script text by
# reading stdin.
set -euo pipefail

SCRIPT_VERSION="unreleased"

# The one `docker inspect --format` template the report uses. It prints only
# name, state and origin labels, never the environment.
INSPECT_FORMAT='{{.Name}}|{{.State.Status}}|{{if eq (index .Config.Labels "net.unraid.docker.managed") "dockerman"}}dockerman{{else if index .Config.Labels "com.docker.compose.project"}}compose{{else}}manual{{end}}|{{index .Config.Labels "com.docker.compose.project"}}'
PS_SIZE_FORMAT='{{.Names}}|{{.Size}}'

ROOT="${HOSERVA_UNRAID_ROOT:-}"
BOOT="$ROOT/boot"
CAP="$BOOT/config/hoserva"
EMHTTP="$ROOT/var/local/emhttp"
AUTOSTART_FILE="$ROOT/var/lib/docker/unraid-autostart"
VERSION_FILE="$ROOT/etc/unraid-version"

MUST_FIX=()
NOTES_ABOUT_SECRETS="The capture in /boot/config/hoserva/ contains your containers' environment variables, secrets included, like the templates on the same flash. The Flash Backup zip is as sensitive as the flash itself: keep it like the flash."

die() {
  printf 'prepare-migration: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat >&2 <<'EOF'
usage: prepare-migration.sh [--zip]

Captures Unraid's runtime state into /boot/config/hoserva/, prints the
Phase A report on stderr, and with --zip streams /boot as a zip on stdout
(refused when stdout is a terminal). Reads only; writes only
/boot/config/hoserva/.
EOF
}

say() { printf '%s\n' "$*" >&3; }
fix() {
  say "!! $*"
  MUST_FIX+=("$*")
}

json_str() {
  local s=$1
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\n'/\\n}
  s=${s//$'\r'/\\r}
  s=${s//$'\t'/\\t}
  s=${s//[[:cntrl:]]/ }
  printf '"%s"' "$s"
}

trim() {
  local s=$1
  s=${s#"${s%%[![:space:]]*}"}
  s=${s%"${s##*[![:space:]]}"}
  printf '%s' "$s"
}

# Flat key="value" files (var.ini, shares/*.cfg, docker.cfg, domain.cfg).
# Read as data, never sourced.
ini_get() {
  local file=$1 key=$2 line v
  [ -f "$file" ] || return 1
  while IFS= read -r line || [ -n "$line" ]; do
    line=${line%$'\r'}
    case $line in
      "$key="*)
        v=${line#*=}
        v=${v#\"}
        v=${v%\"}
        printf '%s' "$v"
        return 0
        ;;
    esac
  done <"$file"
  return 1
}

human_kib() {
  local k=$1
  if ((k >= 1073741824)); then
    printf '%d.%d TiB' $((k / 1073741824)) $((k * 10 / 1073741824 % 10))
  elif ((k >= 1048576)); then
    printf '%d.%d GiB' $((k / 1048576)) $((k * 10 / 1048576 % 10))
  elif ((k >= 1024)); then
    printf '%d.%d MiB' $((k / 1024)) $((k * 10 / 1024 % 10))
  else
    printf '%d KiB' "$k"
  fi
}

# Docker prints sizes in decimal units: "0B", "1.5kB", "12.3MB (virtual 1GB)".
docker_size_bytes() {
  local s=${1%% *} re='^([0-9]+)(\.([0-9]+))?([kKMGTPE]?)B$'
  [[ $s =~ $re ]] || return 1
  local int=${BASH_REMATCH[1]} frac=${BASH_REMATCH[3]} unit=${BASH_REMATCH[4]}
  local mult=1 scale=1 i
  case $unit in
    k | K) mult=1000 ;;
    M) mult=1000000 ;;
    G) mult=1000000000 ;;
    T) mult=1000000000000 ;;
    P) mult=1000000000000000 ;;
    E) mult=1000000000000000000 ;;
  esac
  for ((i = 0; i < ${#frac}; i++)); do scale=$((scale * 10)); done
  printf '%d' $(((int * scale + 10#${frac:-0}) * mult / scale))
}

lsblk_field() {
  local line=$1 key=$2 re="(^| )$2=\"([^\"]*)\""
  if [[ $line =~ $re ]]; then
    local v=${BASH_REMATCH[2]}
    printf '%s' "${v//\\x20/ }"
  fi
}

# ---------------------------------------------------------------- pools

POOLS=()
BOOT_POOLS=()
DISKS=()

discover_storage() {
  local f name size d
  shopt -s nullglob
  for f in "$BOOT"/config/pools/*.cfg; do
    name=${f##*/}
    name=${name%.cfg}
    POOLS+=("$name")
    size=$(ini_get "$f" diskBootSize || true)
    if [[ $size =~ ^[0-9]+$ ]] && ((10#$size > 0)); then
      BOOT_POOLS+=("$name")
    fi
  done
  if [ -d "$ROOT/mnt/cache" ]; then
    local have=0
    for name in "${POOLS[@]}"; do [ "$name" = cache ] && have=1; done
    ((have)) || POOLS+=(cache)
  fi
  for d in "$ROOT"/mnt/disk[0-9]*; do
    if [ -d "$d" ]; then DISKS+=("${d##*/}"); fi
  done
  shopt -u nullglob
}

is_boot_pool() {
  local p
  for p in "${BOOT_POOLS[@]}"; do [ "$p" = "$1" ] && return 0; done
  return 1
}

# Prints every existing physical copy of a configured path, as
# "<class>|<path under ROOT>". Class is boot-pool, cache, array or other.
physical_copies() {
  local p=$1 rest pool d
  case $p in
    /mnt/user/*)
      rest=${p#/mnt/user/}
      for pool in "${POOLS[@]}"; do emit_copy "$(pool_class "$pool")" "$ROOT/mnt/$pool/$rest"; done
      for d in "${DISKS[@]}"; do emit_copy array "$ROOT/mnt/$d/$rest"; done
      ;;
    /mnt/user0/*)
      rest=${p#/mnt/user0/}
      for d in "${DISKS[@]}"; do emit_copy array "$ROOT/mnt/$d/$rest"; done
      ;;
    /mnt/*)
      rest=${p#/mnt/}
      d=${rest%%/*}
      if is_pool "$d"; then
        emit_copy "$(pool_class "$d")" "$ROOT$p"
      elif [[ $d =~ ^disk[0-9]+$ ]]; then
        emit_copy array "$ROOT$p"
      else
        emit_copy other "$ROOT$p"
      fi
      ;;
    *) emit_copy other "$ROOT$p" ;;
  esac
}

emit_copy() {
  if [ -e "$2" ] || [ -L "$2" ]; then printf '%s|%s\n' "$1" "$2"; fi
}

is_pool() {
  local p
  for p in "${POOLS[@]}"; do [ "$p" = "$1" ] && return 0; done
  return 1
}

pool_class() {
  if is_boot_pool "$1"; then printf 'boot-pool'; else printf 'cache'; fi
}

# One class per configured path: the most exposed one when copies exist in
# several places (boot-pool, then cache, then array).
location_class() {
  local path=$1 line seen_boot=0 seen_cache=0 seen_array=0 seen_other=0
  [ -n "$path" ] || {
    printf 'none'
    return 0
  }
  while IFS= read -r line; do
    case ${line%%|*} in
      boot-pool) seen_boot=1 ;;
      cache) seen_cache=1 ;;
      array) seen_array=1 ;;
      other) seen_other=1 ;;
    esac
  done < <(physical_copies "$path")
  if ((seen_boot)); then
    printf 'boot-pool'
  elif ((seen_cache)); then
    printf 'cache'
  elif ((seen_array)); then
    printf 'array'
  elif ((seen_other)); then
    printf 'other'
  else
    printf 'unknown'
  fi
}

# --------------------------------------------------------------- docker

DOCKER_STATE=stopped
DOCKER_DETAIL=""
CONTAINERS_JSON=""
NETWORKS_JSON=""
C_NAMES=()
C_STATES=()
C_ORIGINS=()
C_PROJECTS=()
declare -A C_LAYER_BYTES

collect_docker() {
  local out ids=() line name state origin project size bytes
  if ! command -v docker >/dev/null 2>&1; then
    DOCKER_DETAIL="the docker command is not available"
    return 0
  fi
  if ! out=$(docker ps -aq 2>&1); then
    DOCKER_DETAIL="docker ps failed: the Docker service is not running"
    return 0
  fi
  if [ -n "$out" ]; then
    mapfile -t ids <<<"$out"
  fi

  if ((${#ids[@]} == 0)); then
    CONTAINERS_JSON='[]'
  else
    if ! CONTAINERS_JSON=$(docker inspect "${ids[@]}" 2>/dev/null); then
      DOCKER_STATE=error
      DOCKER_DETAIL="docker inspect of the containers failed"
      return 0
    fi
    if ! out=$(docker inspect --format "$INSPECT_FORMAT" "${ids[@]}" 2>/dev/null); then
      DOCKER_STATE=error
      DOCKER_DETAIL="docker inspect --format failed"
      return 0
    fi
    while IFS= read -r line || [ -n "$line" ]; do
      IFS='|' read -r name state origin project <<<"$line"
      C_NAMES+=("${name#/}")
      C_STATES+=("$state")
      C_ORIGINS+=("$origin")
      C_PROJECTS+=("$project")
    done <<<"$out"
    if ((${#C_NAMES[@]} != ${#ids[@]})); then
      DOCKER_STATE=error
      DOCKER_DETAIL="docker inspect returned ${#C_NAMES[@]} summaries for ${#ids[@]} containers"
      return 0
    fi
    if ! out=$(docker ps -a -s --format "$PS_SIZE_FORMAT" 2>/dev/null); then
      DOCKER_STATE=error
      DOCKER_DETAIL="docker ps -s failed"
      return 0
    fi
    while IFS= read -r line || [ -n "$line" ]; do
      IFS='|' read -r name size <<<"$line"
      if bytes=$(docker_size_bytes "$size"); then
        C_LAYER_BYTES["$name"]=$bytes
      else
        C_LAYER_BYTES["$name"]=""
      fi
    done <<<"$out"
  fi

  if ! out=$(docker network ls -q 2>/dev/null); then
    DOCKER_STATE=error
    DOCKER_DETAIL="docker network ls failed"
    return 0
  fi
  ids=()
  if [ -n "$out" ]; then
    mapfile -t ids <<<"$out"
  fi
  if ((${#ids[@]} == 0)); then
    NETWORKS_JSON='[]'
  elif ! NETWORKS_JSON=$(docker network inspect "${ids[@]}" 2>/dev/null); then
    DOCKER_STATE=error
    DOCKER_DETAIL="docker network inspect failed"
    return 0
  fi
  DOCKER_STATE=running
}

# -------------------------------------------------------------- capture

write_capture_file() {
  local name=$1 tmp="$CAP/.$1.tmp"
  cat >"$tmp" || return 1
  mv -f -- "$tmp" "$CAP/$name" || return 1
}

# shellcheck disable=SC2317 # called from the EXIT trap
cleanup_tmp() {
  local f
  shopt -s nullglob dotglob
  for f in "$CAP"/.*.tmp; do rm -f -- "$f"; done
  shopt -u nullglob dotglob
}

# Removes only the files this script owns inside the capture directory, so a
# rerun never leaves an earlier run's file beside one it did not write.
reset_capture() {
  local f
  mkdir -p -- "$CAP" || die "cannot create $CAP (is the flash writable?)"
  for f in containers.json networks.json disks.ini autostart var.ini capture.json report.txt; do
    rm -f -- "$CAP/$f"
  done
  if [ -d "$CAP/smart" ]; then
    shopt -s nullglob dotglob
    for f in "$CAP"/smart/*; do rm -f -- "$f"; done
    shopt -u nullglob dotglob
    rmdir -- "$CAP/smart" 2>/dev/null || true
  fi
}

copy_capture_file() {
  local src=$1 name=$2
  if [ -f "$src" ]; then
    write_capture_file "$name" <"$src" || die "cannot write $CAP/$name"
    return 0
  fi
  return 1
}

capture_var_subset() {
  local key v out=""
  for key in sbSynced sbSyncExit sbSyncErrs mdResync; do
    if v=$(ini_get "$EMHTTP/var.ini" "$key"); then
      out+="$key=\"$v\""$'\n'
    fi
  done
  if [ -n "$out" ]; then
    printf '%s' "$out" | write_capture_file var.ini || die "cannot write $CAP/var.ini"
  fi
}

capture_smart() {
  local f
  [ -d "$EMHTTP/smart" ] || return 0
  mkdir -p -- "$CAP/smart" || die "cannot create $CAP/smart"
  shopt -s nullglob dotglob
  for f in "$EMHTTP"/smart/*; do
    if [ -f "$f" ] && [ ! -L "$f" ]; then
      cat -- "$f" >"$CAP/smart/${f##*/}" || die "cannot write $CAP/smart/${f##*/}"
    fi
  done
  shopt -u nullglob dotglob
}

# ----------------------------------------------------------------- boot

BOOT_MODE=unknown
BOOT_FSTYPE=""
BOOT_SOURCE=""
BOOT_MIRRORED=false
BOOT_SHARED=false
BOOT_DEVICES_JSON='[]'
BOOT_DEVICE_COUNT=0
LSBLK_ROWS=()

first_line() { printf '%s' "${1%%$'\n'*}"; }

analyze_boot() {
  local out row name pk typ pool fstype label
  local -A seen=()
  local devs=()

  if out=$(findmnt -no FSTYPE "$BOOT" 2>/dev/null); then BOOT_FSTYPE=$(first_line "$out"); fi
  if out=$(findmnt -no SOURCE "$BOOT" 2>/dev/null); then BOOT_SOURCE=$(first_line "$out"); fi
  case $BOOT_FSTYPE in
    vfat | msdos | exfat) BOOT_MODE=usb ;;
    zfs) BOOT_MODE=internal ;;
  esac

  if out=$(lsblk -P -o NAME,TYPE,PKNAME,SERIAL,MODEL,FSTYPE,LABEL,SIZE 2>/dev/null) && [ -n "$out" ]; then
    mapfile -t LSBLK_ROWS <<<"$out"
  fi

  case $BOOT_MODE in
    usb)
      name=${BOOT_SOURCE##*/}
      for row in "${LSBLK_ROWS[@]}"; do
        if [ "$(lsblk_field "$row" NAME)" = "$name" ]; then
          pk=$(lsblk_field "$row" PKNAME)
          typ=$(lsblk_field "$row" TYPE)
          [ -n "$pk" ] || { [ "$typ" = disk ] && pk=$name; }
          [ -n "$pk" ] && devs+=("$pk")
        fi
      done
      ;;
    internal)
      pool=${BOOT_SOURCE%%/*}
      for row in "${LSBLK_ROWS[@]}"; do
        fstype=$(lsblk_field "$row" FSTYPE)
        label=$(lsblk_field "$row" LABEL)
        if [ "$fstype" = zfs_member ] && [ -n "$pool" ] && [ "$label" = "$pool" ]; then
          pk=$(lsblk_field "$row" PKNAME)
          [ -n "$pk" ] && devs+=("$pk")
        fi
      done
      ;;
  esac

  local json="" d first=1
  for d in "${devs[@]}"; do
    [ -n "${seen[$d]:-}" ] && continue
    seen[$d]=1
    BOOT_DEVICE_COUNT=$((BOOT_DEVICE_COUNT + 1))
    for row in "${LSBLK_ROWS[@]}"; do
      if [ "$(lsblk_field "$row" NAME)" = "$d" ]; then
        ((first)) || json+=","
        first=0
        json+="{\"name\":$(json_str "$d"),\"serial\":$(json_str "$(lsblk_field "$row" SERIAL)"),\"model\":$(json_str "$(lsblk_field "$row" MODEL)"),\"size\":$(json_str "$(lsblk_field "$row" SIZE)")}"
        break
      fi
    done
  done
  BOOT_DEVICES_JSON="[$json]"

  if [ "$BOOT_MODE" = internal ]; then
    if ((BOOT_DEVICE_COUNT > 1)); then BOOT_MIRRORED=true; fi
    if ((${#BOOT_POOLS[@]} > 0)); then BOOT_SHARED=true; fi
    for d in "${!seen[@]}"; do
      for row in "${LSBLK_ROWS[@]}"; do
        if [ "$(lsblk_field "$row" PKNAME)" = "$d" ]; then
          fstype=$(lsblk_field "$row" FSTYPE)
          label=$(lsblk_field "$row" LABEL)
          case $fstype in
            btrfs | xfs | ext4 | zfs_member)
              [ "$label" = "${BOOT_SOURCE%%/*}" ] || BOOT_SHARED=true
              ;;
          esac
        fi
      done
    done
  fi
}

# ------------------------------------------------------- docker location

DOCKER_PATH=""
DOCKER_LOCATION=none
LIBVIRT_PATH=""
LIBVIRT_LOCATION=none

analyze_locations() {
  local img
  img=$(ini_get "$BOOT/config/docker.cfg" DOCKER_IMAGE_FILE || true)
  while [ "${img%/}" != "$img" ]; do img=${img%/}; done
  DOCKER_PATH=$img
  DOCKER_LOCATION=$(location_class "$DOCKER_PATH")
  LIBVIRT_PATH=$(ini_get "$BOOT/config/domain.cfg" IMAGE_FILE || true)
  LIBVIRT_LOCATION=$(location_class "$LIBVIRT_PATH")
}

write_capture_json() {
  local now first=1 name bytes layers
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  local ver
  ver=$(ini_get "$VERSION_FILE" version || true)
  if [ "$DOCKER_STATE" = running ]; then
    layers=""
    for name in "${C_NAMES[@]}"; do
      bytes=${C_LAYER_BYTES[$name]:-}
      ((first)) || layers+=","
      first=0
      layers+="{\"container\":$(json_str "$name"),\"bytes\":${bytes:-null}}"
    done
    layers="[$layers]"
  else
    layers=null
  fi
  {
    printf '{\n'
    printf '  "captured_at": %s,\n' "$(json_str "$now")"
    printf '  "unraid_version": %s,\n' "$(json_str "$ver")"
    printf '  "script_version": %s,\n' "$(json_str "$SCRIPT_VERSION")"
    printf '  "boot": {"mode": %s, "filesystem": %s, "devices": %s, "mirrored": %s, "shared_with_data_pool": %s},\n' \
      "$(json_str "$BOOT_MODE")" "$(json_str "$BOOT_FSTYPE")" "$BOOT_DEVICES_JSON" "$BOOT_MIRRORED" "$BOOT_SHARED"
    printf '  "docker": {"state": %s, "directory_location": %s, "writable_layers": %s},\n' \
      "$(json_str "$DOCKER_STATE")" "$(json_str "$DOCKER_LOCATION")" "$layers"
    printf '  "libvirt_img_location": %s\n' "$(json_str "$LIBVIRT_LOCATION")"
    printf '}\n'
  } | write_capture_file capture.json || die "cannot write $CAP/capture.json"
}

# --------------------------------------------------------------- report

report_containers() {
  local i n_dm=0 n_compose=0 n_manual=0 n_run=0 n_other=0
  declare -A tmpl=()
  local tmpl_dir="$BOOT/config/plugins/dockerMan/templates-user" f c n re='<Name>([^<]*)</Name>'

  say "== Containers =="
  case $DOCKER_STATE in
    stopped)
      fix "Docker is not running ($DOCKER_DETAIL). Start the Docker service in Unraid and run this script again: without it there is no container list and no template check."
      say
      return 0
      ;;
    error)
      fix "Docker did not answer completely ($DOCKER_DETAIL). The container capture was not written. Run this script again."
      say
      return 0
      ;;
  esac

  shopt -s nullglob
  for f in "$tmpl_dir"/*.xml; do
    [ -f "$f" ] || continue
    c=$(<"$f") || continue
    if [[ $c =~ $re ]]; then
      n=$(trim "${BASH_REMATCH[1]}")
      tmpl["$n"]=1
    fi
  done
  shopt -u nullglob

  for ((i = 0; i < ${#C_NAMES[@]}; i++)); do
    case ${C_ORIGINS[$i]} in
      dockerman) n_dm=$((n_dm + 1)) ;;
      compose) n_compose=$((n_compose + 1)) ;;
      *) n_manual=$((n_manual + 1)) ;;
    esac
    if [ "${C_STATES[$i]}" = running ]; then n_run=$((n_run + 1)); else n_other=$((n_other + 1)); fi
  done
  say "${#C_NAMES[@]} containers: $n_run running, $n_other not running."
  say "By origin: $n_dm from the Docker page (dockerMan templates), $n_compose from Compose Manager, $n_manual created by hand."
  for ((i = 0; i < ${#C_NAMES[@]}; i++)); do
    case ${C_ORIGINS[$i]} in
      dockerman) n="dockerMan" ;;
      compose) n="Compose Manager project ${C_PROJECTS[$i]}" ;;
      *) n="created by hand" ;;
    esac
    say "  - ${C_NAMES[$i]}  [${C_STATES[$i]}]  $n"
  done

  local missing=()
  for ((i = 0; i < ${#C_NAMES[@]}; i++)); do
    if [ "${C_ORIGINS[$i]}" = dockerman ] && [ -z "${tmpl[${C_NAMES[$i]}]:-}" ]; then
      missing+=("${C_NAMES[$i]}")
    fi
  done
  if ((${#missing[@]} > 0)); then
    fix "${#missing[@]} dockerMan container(s) have no template whose <Name> matches: ${missing[*]}. They cannot be converted. Fix it while Unraid runs: open each one on the Docker page, edit it and apply (that saves its template), or re-add it from its template."
  fi
  if ((n_manual > 0)); then
    local manual=()
    for ((i = 0; i < ${#C_NAMES[@]}; i++)); do
      [ "${C_ORIGINS[$i]}" = dockerman ] || [ "${C_ORIGINS[$i]}" = compose ] || manual+=("${C_NAMES[$i]}")
    done
    say "Created by hand (no template to convert; recreate them from their run command): ${manual[*]}"
  fi
  if ((n_compose > 0)); then
    say "Compose Manager containers come from the project's own compose.yaml and are offered as it is."
  fi
  say
}

report_autostart() {
  local line name delay i=0 rest
  say "== Autostart order =="
  if [ ! -f "$AUTOSTART_FILE" ]; then
    say "No autostart list (/var/lib/docker/unraid-autostart) was found: no container starts with the array."
    say
    return 0
  fi
  while IFS= read -r line || [ -n "$line" ]; do
    line=${line%$'\r'}
    [ -n "$(trim "$line")" ] || continue
    read -r name delay rest <<<"$line"
    i=$((i + 1))
    if [ -n "${delay:-}" ]; then
      say "  $i. $name (waits $delay s before the next)"
    else
      say "  $i. $name"
    fi
  done <"$AUTOSTART_FILE"
  ((i > 0)) || say "The autostart list is empty."
  say
}

SHARE_NAMES=()
declare -A SHARE_CACHE SHARE_FLOOR
CACHE_ROWS=()
CACHE_TOTAL_KIB=0
CACHE_UNKNOWN=0
CACHE_MAX_FLOOR=0

load_shares() {
  local f name v
  shopt -s nullglob
  for f in "$BOOT"/config/shares/*.cfg; do
    name=${f##*/}
    name=${name%.cfg}
    SHARE_NAMES+=("$name")
    SHARE_CACHE["$name"]=$(ini_get "$f" shareUseCache || true)
    v=$(ini_get "$f" shareFloor || true)
    [[ $v =~ ^[0-9]+$ ]] || v=0
    SHARE_FLOOR["$name"]=$v
  done
  shopt -u nullglob
}

# Docker's own storage (the directory, or docker.img) is not moved, so it is
# left out of both the "is there data" test and the size.
docker_exclusion() {
  local pool=$1 rest
  case $DOCKER_PATH in
    /mnt/user/*)
      rest=${DOCKER_PATH#/mnt/user/}
      printf '%s' "$ROOT/mnt/$pool/$rest"
      ;;
    /mnt/"$pool"/*) printf '%s' "$ROOT$DOCKER_PATH" ;;
  esac
}

analyze_cache() {
  local pool base dir name excl found reason setting kib out floor
  shopt -s nullglob
  for pool in "${POOLS[@]}"; do
    base="$ROOT/mnt/$pool"
    [ -d "$base" ] || continue
    excl=$(docker_exclusion "$pool")
    for dir in "$base"/*/; do
      dir=${dir%/}
      name=${dir##*/}
      [ "$name" = lost+found ] && continue
      if [ -n "$excl" ]; then
        found=$(find "$dir" -path "$excl" -prune -o ! -type d -print -quit 2>/dev/null) || found="?"
      else
        found=$(find "$dir" ! -type d -print -quit 2>/dev/null) || found="?"
      fi
      [ -n "$found" ] || continue

      setting=${SHARE_CACHE[$name]:-}
      reason=""
      case $setting in
        prefer | only) reason="share cache setting \"$setting\"" ;;
      esac
      case $name in
        appdata | domains | system) reason=${reason:+$reason; }"the $name directory is on the cache" ;;
      esac
      if [ -z "$reason" ]; then
        if [ -n "${SHARE_CACHE[$name]+set}" ]; then
          reason="share cache setting \"${setting:-unset}\" (the mover has not moved it)"
        else
          reason="no share configuration (a plain directory)"
        fi
      fi

      if [ -n "$excl" ]; then
        out=$(du -sk --exclude="$excl" -- "$dir" 2>/dev/null) || out=""
      else
        out=$(du -sk -- "$dir" 2>/dev/null) || out=""
      fi
      kib=${out%%[[:space:]]*}
      if [[ $kib =~ ^[0-9]+$ ]]; then
        CACHE_TOTAL_KIB=$((CACHE_TOTAL_KIB + kib))
      else
        kib="?"
        CACHE_UNKNOWN=1
      fi
      CACHE_ROWS+=("$name|$pool|$kib|$reason")
      floor=${SHARE_FLOOR[$name]:-0}
      if ((floor > CACHE_MAX_FLOOR)); then CACHE_MAX_FLOOR=$floor; fi
    done
  done
  shopt -u nullglob
}

report_shares() {
  local name d row pool kib reason n
  say "== Shares and the cache =="
  if ((${#SHARE_NAMES[@]} == 0)); then
    say "No share configuration was found in /boot/config/shares."
  else
    say "Shares and their cache setting:"
    for name in "${SHARE_NAMES[@]}"; do
      say "  - $name: cache \"${SHARE_CACHE[$name]:-unset}\""
    done
  fi

  if ((${#POOLS[@]} == 0)); then
    say "No cache or pool was found."
  fi

  if ((${#CACHE_ROWS[@]} > 0)); then
    fix "Data is still on the cache (${#CACHE_ROWS[@]} directories). It is lost when the cache device is re-created or the boot device is reinstalled. Move it to the array first: stop the Docker and VM services, then run the mover or rsync."
    for row in "${CACHE_ROWS[@]}"; do
      IFS='|' read -r name pool kib reason <<<"$row"
      if [ "$kib" = "?" ]; then
        say "     $name on pool $pool: size unknown; $reason"
      else
        say "     $name on pool $pool: $(human_kib "$kib"); $reason"
      fi
    done
  fi

  if [ "$LIBVIRT_LOCATION" = cache ] || [ "$LIBVIRT_LOCATION" = boot-pool ]; then
    fix "libvirt.img ($LIBVIRT_PATH) is on the cache: stop the VM service and move it to the array."
  fi

  case $DOCKER_LOCATION in
    none) say "Docker storage: not configured." ;;
    cache | boot-pool)
      say "Docker storage ($DOCKER_PATH) is on the cache. It is not moved: images are pulled again when the containers are recreated. What does not come back is each container's writable layer, listed below."
      ;;
    array) say "Docker storage ($DOCKER_PATH) is on the array." ;;
    other) say "Docker storage ($DOCKER_PATH) is outside the array and the pools." ;;
    *) say "Docker storage ($DOCKER_PATH) was not found on the array or a pool, so its location is not recorded." ;;
  esac
  if [ "$DOCKER_STATE" = running ] && ((${#C_NAMES[@]} > 0)); then
    say "Writable-layer size per container (state kept inside the container, not in a mapped path):"
    local i bytes
    for ((i = 0; i < ${#C_NAMES[@]}; i++)); do
      bytes=${C_LAYER_BYTES[${C_NAMES[$i]}]:-}
      if [[ $bytes =~ ^[0-9]+$ ]]; then
        say "  - ${C_NAMES[$i]}: $(human_kib $(((bytes + 1023) / 1024)))"
      else
        say "  - ${C_NAMES[$i]}: unknown"
      fi
    done
  fi

  if ((${#CACHE_ROWS[@]} > 0)); then
    local free_total=0 usable_total=0 free_kib line avail dfail=0
    for d in "${DISKS[@]}"; do
      if line=$(df -Pk -- "$ROOT/mnt/$d" 2>/dev/null | sed -n '2p') && read -r _ _ _ avail _ <<<"$line" && [[ ${avail:-} =~ ^[0-9]+$ ]]; then
        free_kib=$avail
        free_total=$((free_total + free_kib))
        if ((free_kib > CACHE_MAX_FLOOR)); then
          usable_total=$((usable_total + free_kib - CACHE_MAX_FLOOR))
        fi
      else
        dfail=1
        say "     could not read the free space of $d"
      fi
    done
    n=${#DISKS[@]}
    if ((CACHE_UNKNOWN)); then
      fix "The size of some of that data could not be measured, so whether it fits on the array is unknown."
    elif ((dfail)) || ((n == 0)); then
      fix "The free space of the array could not be read (${n} data disks), so whether the cache data fits on it is unknown."
    elif ((CACHE_TOTAL_KIB > usable_total)); then
      fix "The data on the cache ($(human_kib "$CACHE_TOTAL_KIB")) does NOT fit on the array: $(human_kib "$usable_total") usable of $(human_kib "$free_total") free across $n data disks, after the largest share floor ($(human_kib "$CACHE_MAX_FLOOR")) is kept free on each. Free up space on the array first."
    else
      say "Fit: the data on the cache ($(human_kib "$CACHE_TOTAL_KIB")) fits on the array: $(human_kib "$usable_total") usable of $(human_kib "$free_total") free across $n data disks, after the largest share floor ($(human_kib "$CACHE_MAX_FLOOR")) is kept free on each. This compares totals; it does not check that one large file fits on one disk."
    fi
  elif ((${#POOLS[@]} > 0)); then
    say "Nothing is left on the cache to move."
  fi
  say
}

report_parity() {
  local synced exit_code errs running when
  say "== Last parity check =="
  synced=$(ini_get "$EMHTTP/var.ini" sbSynced || true)
  exit_code=$(ini_get "$EMHTTP/var.ini" sbSyncExit || true)
  errs=$(ini_get "$EMHTTP/var.ini" sbSyncErrs || true)
  running=$(ini_get "$EMHTTP/var.ini" mdResync || true)
  if [ -z "$synced" ] && [ -z "$exit_code" ]; then
    fix "The parity-check result could not be read from var.ini. Check the last parity check in Unraid and make sure it completed clean."
    say
    return 0
  fi
  if [[ $synced =~ ^[0-9]+$ ]] && ((10#$synced > 0)); then
    when=$(date -u -d "@$synced" '+%Y-%m-%d %H:%M UTC' 2>/dev/null || printf 'unknown date')
  else
    when="never"
  fi
  say "Last parity check: $when, exit code ${exit_code:-unknown}, ${errs:-unknown} errors."
  if [[ $running =~ ^[0-9]+$ ]] && ((10#$running > 0)); then
    fix "A parity check or sync is running right now. Let it finish and confirm it is clean."
  fi
  if [ "$when" = never ]; then
    fix "No parity check has completed on this array. Run one and confirm it is clean before migrating."
  elif [ "${exit_code:-}" != 0 ]; then
    fix "The last parity check did not finish clean (exit code ${exit_code:-unknown}). Run a full check and confirm it completes before migrating."
  elif [ "${errs:-}" != 0 ]; then
    fix "The last parity check found ${errs:-unknown} errors. Resolve them and run another check before migrating."
  fi
  say
}

D_NAME="" D_TYPE="" D_ID="" D_DEVICE="" D_STATUS=""
DISK_ROWS=()

d_flush() {
  if [ -n "$D_NAME" ]; then
    DISK_ROWS+=("$D_NAME|$D_TYPE|$D_ID|$D_DEVICE|$D_STATUS")
  fi
  D_NAME="" D_TYPE="" D_ID="" D_DEVICE="" D_STATUS=""
}

load_disks() {
  local line k v
  [ -f "$EMHTTP/disks.ini" ] || return 0
  while IFS= read -r line || [ -n "$line" ]; do
    line=${line%$'\r'}
    case $line in
      \[*\])
        d_flush
        v=${line#\[}
        v=${v%\]}
        v=${v#\"}
        v=${v%\"}
        D_NAME=$v
        ;;
      *=*)
        k=${line%%=*}
        v=${line#*=}
        v=${v#\"}
        v=${v%\"}
        case $k in
          type) D_TYPE=$v ;;
          id) D_ID=$v ;;
          device) D_DEVICE=$v ;;
          status) D_STATUS=$v ;;
        esac
        ;;
    esac
  done <"$EMHTTP/disks.ini"
  d_flush
}

report_disks() {
  local row name type id device status parity=()
  say "== Disks: serial and slot =="
  if [ ! -f "$EMHTTP/disks.ini" ]; then
    fix "disks.ini was not found under /var/local/emhttp, so the serial to slot table could not be captured. Note it by hand from Main in the Unraid UI."
    say
    return 0
  fi
  for row in "${DISK_ROWS[@]}"; do
    IFS='|' read -r name type id device status <<<"$row"
    say "  - $name ($type): ${id:-(empty slot)}${device:+ as $device}${status:+, $status}"
    case $type in
      Parity | parity) parity+=("$name") ;;
    esac
  done
  if ((${#parity[@]} > 0)); then
    say "Parity disk(s): ${parity[*]}. Their contents are not kept: they are rewritten as parity."
  else
    say "No parity disk is assigned."
  fi
  say
}

report_boot() {
  say "== Boot device and rollback =="
  case $BOOT_MODE in
    usb)
      say "Unraid boots from a USB stick ($BOOT_FSTYPE). Keep the stick safe at step 11 of the migration: it is the rollback."
      say "Rollback after Debian is installed:"
      say "  - Debian on a separate device: reinsert the stick and boot."
      say "  - Debian on the shared NVMe (with the Unraid cache): reinsert the stick, re-create the Unraid cache, and move appdata back."
      ;;
    internal)
      say "Unraid boots from an internal boot pool ($BOOT_FSTYPE), $BOOT_DEVICE_COUNT device(s)$([ "$BOOT_MIRRORED" = true ] && printf ', mirrored')."
      if [ "$BOOT_SHARED" = true ]; then
        say "The boot device also holds a data pool (boot and data on one device)."
        say "Rollback after Debian is installed:"
        say "  - Debian on the same NVMe: restore the Flash Backup zip to a USB stick (USB Flash Creator) and boot it; re-create the cache."
      else
        say "The boot pool has its own device(s)."
        say "Rollback after Debian is installed:"
        say "  - Debian on another device: switch the firmware boot order back."
        say "  - Debian on this boot device: restore the Flash Backup zip to a USB stick (USB Flash Creator) and boot it."
      fi
      say "There is no stick to keep: the Flash Backup zip is the rollback artifact."
      ;;
    *)
      fix "The boot mode could not be determined (/boot is '${BOOT_FSTYPE:-not a mount point}'). Find your row in the rollback table of the migration guide before you cross step 12."
      ;;
  esac
  say
}

build_report() {
  local ver when
  ver=$(ini_get "$VERSION_FILE" version || true)
  when=$(date -u '+%Y-%m-%d %H:%M UTC')
  say "Hoserva Phase A report (prepare-migration $SCRIPT_VERSION, $when)"
  say "Unraid ${ver:-unknown version}. Lines starting with !! must be fixed while Unraid still runs."
  say
  report_containers
  report_autostart
  report_shares
  report_parity
  report_disks
  report_boot
  say "== Capture =="
  say "Written to /boot/config/hoserva/: containers.json, networks.json, disks.ini, autostart, var.ini, smart/, capture.json and this report."
  say "Docker files are absent when Docker was not running; the capture says so."
  say "$NOTES_ABOUT_SECRETS"
  say
  if ((${#MUST_FIX[@]} == 0)); then
    say "Nothing to fix. Continue with the migration steps."
  else
    say "To fix before you continue (${#MUST_FIX[@]}):"
    local m
    for m in "${MUST_FIX[@]}"; do say "  !! $m"; done
  fi
}

# ------------------------------------------------------------------ zip

stream_zip() {
  local entries=() f name
  shopt -s nullglob dotglob
  for f in "$BOOT"/*; do
    name=${f##*/}
    case $name in
      previous | prev) continue ;;
      -*)
        shopt -u nullglob dotglob
        die "refusing to zip: top-level entry '$name' starts with '-' and would be read as a zip option"
        ;;
    esac
    entries+=("$name")
  done
  shopt -u nullglob dotglob
  ((${#entries[@]} > 0)) || die "nothing to zip under $BOOT"
  (cd -- "$BOOT" && zip -qr - "${entries[@]}") || die "zip failed: the stream is incomplete, do not use it"
}

# ----------------------------------------------------------------- main

main() {
  local want_zip=0 arg
  exec </dev/null
  for arg in "$@"; do
    case $arg in
      --zip) want_zip=1 ;;
      -h | --help)
        usage
        return 0
        ;;
      --version)
        printf '%s\n' "$SCRIPT_VERSION"
        return 0
        ;;
      *)
        usage
        return 2
        ;;
    esac
  done

  if ((want_zip)) && [ -t 1 ]; then
    die "--zip writes a zip to stdout; stdout is a terminal. Redirect it to a file: ... --zip > <server>-boot.zip"
  fi
  if ((want_zip)); then
    command -v zip >/dev/null 2>&1 || die "--zip needs the zip command, which was not found"
  fi
  [ -f "$VERSION_FILE" ] || die "this does not look like an Unraid server ($VERSION_FILE is missing)"
  [ -d "$BOOT/config" ] || die "$BOOT/config is missing: is the flash mounted?"

  trap cleanup_tmp EXIT
  reset_capture
  exec 3>"$CAP/.report.tmp" || die "cannot write the report under $CAP"

  discover_storage
  collect_docker
  analyze_boot
  analyze_locations
  load_shares
  load_disks
  analyze_cache

  if [ "$DOCKER_STATE" = running ]; then
    printf '%s\n' "$CONTAINERS_JSON" | write_capture_file containers.json || die "cannot write $CAP/containers.json"
    printf '%s\n' "$NETWORKS_JSON" | write_capture_file networks.json || die "cannot write $CAP/networks.json"
  fi
  copy_capture_file "$EMHTTP/disks.ini" disks.ini || true
  copy_capture_file "$AUTOSTART_FILE" autostart || true
  capture_var_subset
  capture_smart
  write_capture_json

  build_report
  exec 3>&-
  mv -f -- "$CAP/.report.tmp" "$CAP/report.txt" || die "cannot write $CAP/report.txt"
  cat -- "$CAP/report.txt" >&2

  if ((want_zip)); then
    stream_zip
  fi
  return 0
}

main "$@"; exit $?
