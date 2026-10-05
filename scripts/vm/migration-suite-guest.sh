#!/usr/bin/env bash
# Runs inside the L3 guest as root, started by run-migration-suite.sh (doc 06
# §5, issue #80): the checks that read the migrated array file by file. Each one
# compares what the guest holds with the fixture's recorded result
# (/srv/unraid-fixtures/<variant>/expected/), which the builder wrote before
# Hoserva existed on the guest.
#
#   files      <expected> <root> <pool-slots>            every file of every data disk is at <root>/<path>
#                                                        with a sha256 the manifest records for that path
#   owners     <expected> <root> <pool-slots>            owner and mode of every file, owner of every
#                                                        directory and symlink, each at <root>/<disk>/<path>
#   relocated  <expected> <cache> <share> <pool-slots>   what a share relocation to the cache carries: files
#                                                        by sha256, symlinks, FIFOs, device nodes and sparse
#                                                        files, and none of it left on its data disk
#
# <expected> is the fixture's expected/ directory. <pool-slots> is a comma
# separated list of the fixture's pool and boot slots, whose rows are skipped:
# the point of no return erases them; '-' says there are none. Every check prints what it found and exits
# non-zero on the first kind of difference, after listing the first 20.
set -euo pipefail
export LC_ALL=C

die() {
  printf 'migration-suite-guest: %s\n' "$*" >&2
  exit 1
}

cmd=${1:-}
[[ -n $cmd ]] || die "usage: migration-suite-guest.sh files|owners|relocated ..."
shift

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

# rows of a fixture file that belong to a data disk: the manifest's, or the
# entries'. $1 is the file, $2 the pool slots, $3 the column holding the disk.
data_rows() {
  local pools=$2
  [[ $pools != - ]] || pools=""
  grep -v '^#' "$1" | awk -F'\t' -v pools=",$pools," -v col="$3" 'index(pools, "," $col ",") == 0'
}

report() {  # title, file of difference lines
  local title=$1 list=$2 n
  n=$(wc -l <"$list")
  if ((n == 0)); then return 0; fi
  printf '%s: %d\n' "$title" "$n"
  head -n 20 -- "$list" | sed 's/^/  /'
  if ((n > 20)); then printf '  ... and %d more\n' "$((n - 20))"; fi
  return 1
}

cmd_files() {
  local exp=$1 root=$2 pools=$3 total bad=0
  [[ -d $root ]] || die "$root is not a directory"
  data_rows "$exp/manifest.sha256" "$pools" 5 >"$work/rows"
  total=$(wc -l <"$work/rows")
  ((total > 0)) || die "the manifest has no file on a data disk"
  awk -F'\t' '{ print $1 "\t" $6 }' "$work/rows" | sort -u >"$work/expected"
  awk -F'\t' '{ print $6 }' "$work/rows" | sort -u | tr '\n' '\0' >"$work/paths"
  (cd -- "$root" && xargs -0 -a "$work/paths" sha256sum -z -- 2>"$work/stderr") >"$work/actual.z" || true
  tr '\0' '\n' <"$work/actual.z" | awk '{ print substr($0, 1, 64) "\t" substr($0, 67) }' >"$work/actual"
  awk -F'\t' 'NR == FNR { ok[$1 "\t" $2] = 1; next } !(($1 "\t" $2) in ok) { print $2 }' "$work/expected" "$work/actual" >"$work/changed"
  awk -F'\t' 'NR == FNR { seen[$2] = 1; next } !($2 in seen) { print $2 }' "$work/actual" "$work/expected" | sort -u >"$work/missing"
  report "files present with another sha256 than the manifest records" "$work/changed" || bad=1
  report "files missing" "$work/missing" || bad=1
  if ((bad)); then return 1; fi
  printf 'all %d files of the data disks are at %s with the sha256 the manifest records\n' "$(tr -cd '\0' <"$work/paths" | wc -c)" "$root"
}

cmd_owners() {
  local exp=$1 root=$2 pools=$3 bad=0
  # each file and directory is read where its disk holds it, <root>/<disk>/<path>:
  # the pool's view of a directory is the first branch's, which the point of no
  # return may have created on the cache
  data_rows "$exp/manifest.sha256" "$pools" 5 >"$work/rows"
  awk -F'\t' '{ print $5 "/" $6 "\t" $4 "\t" $3 }' "$work/rows" | sort -u >"$work/want-files"
  (cd -- "$root" && awk -F'\t' '{ print $1 }' "$work/want-files" | tr '\n' '\0' | xargs -0 stat --printf '%n\t%u:%g\t%a\n' -- 2>/dev/null) | sort -u >"$work/have-files" || true
  awk -F'\t' 'NR == FNR { ok[$0] = 1; next } !($0 in ok) { print $1 }' "$work/have-files" "$work/want-files" >"$work/wrong-files"
  # an entry is a directory or a symlink; its mode is not asserted (the point of
  # no return changes the top level of a share, Q26)
  data_rows "$exp/entries.tsv" "$pools" 2 | awk -F'\t' '$1 == "dir" || $1 == "symlink" { print $2 "/" $3 "\t" $6 }' | sort -u >"$work/want-entries"
  (cd -- "$root" && awk -F'\t' '{ print $1 }' "$work/want-entries" | tr '\n' '\0' | xargs -0 stat --printf '%n\t%u:%g\n' -- 2>/dev/null) | sort -u >"$work/have-entries" || true
  awk -F'\t' 'NR == FNR { ok[$0] = 1; next } !($0 in ok) { print $1 }' "$work/have-entries" "$work/want-entries" >"$work/wrong-entries"
  report "files whose owner or mode differs from the manifest (or that are missing)" "$work/wrong-files" || bad=1
  report "directories and symlinks whose owner differs from the entries (or that are missing)" "$work/wrong-entries" || bad=1
  if ((bad)); then return 1; fi
  printf 'owner and mode of %d files and owner of %d directories and symlinks match the fixture\n' "$(wc -l <"$work/want-files")" "$(wc -l <"$work/want-entries")"
}

cmd_relocated() {
  local exp=$1 cache=$2 share=$3 pools=$4 bad=0 nfiles nentries
  [[ -d $cache/$share ]] || die "$cache/$share does not exist: nothing was relocated"
  # files: on the cache with the recorded sha256, and gone from their disk
  data_rows "$exp/manifest.sha256" "$pools" 5 | awk -F'\t' -v s="$share/" 'index($6, s) == 1' >"$work/rows"
  nfiles=$(wc -l <"$work/rows")
  ((nfiles > 0)) || die "the manifest has no file of $share on a data disk"
  awk -F'\t' '{ print $1 "\t" $6 }' "$work/rows" | sort -u >"$work/expected"
  awk -F'\t' '{ print $6 }' "$work/rows" | sort -u | tr '\n' '\0' >"$work/paths"
  (cd -- "$cache" && xargs -0 -a "$work/paths" sha256sum -z -- 2>/dev/null) >"$work/actual.z" || true
  tr '\0' '\n' <"$work/actual.z" | awk '{ print substr($0, 1, 64) "\t" substr($0, 67) }' >"$work/actual"
  awk -F'\t' 'NR == FNR { ok[$1 "\t" $2] = 1; next } !(($1 "\t" $2) in ok) { print $2 }' "$work/expected" "$work/actual" >"$work/changed"
  awk -F'\t' 'NR == FNR { seen[$2] = 1; next } !($2 in seen) { print $2 }' "$work/actual" "$work/expected" | sort -u >"$work/missing"
  awk -F'\t' '{ print "/mnt/" $5 "/" $6 }' "$work/rows" | while IFS= read -r p; do
    if [[ -e $p || -L $p ]]; then printf '%s\n' "$p"; fi
  done >"$work/left"
  report "relocated files with another sha256 than the manifest records" "$work/changed" || bad=1
  report "files not on the cache" "$work/missing" || bad=1
  report "files still on their data disk" "$work/left" || bad=1

  # entries: symlinks, FIFOs, device nodes, sparse files (a socket is runtime
  # state the relocation reports as skipped and does not carry)
  data_rows "$exp/entries.tsv" "$pools" 2 | awk -F'\t' -v s="$share/" 'index($3, s) == 1 && $1 != "dir" && $1 != "xattr" && $1 != "socket"' >"$work/entries"
  nentries=$(wc -l <"$work/entries")
  : >"$work/entry-bad"
  while IFS=$'\t' read -r type disk path detail _mode _owner; do
    dest=$cache/$path
    case $type in
      symlink)
        [[ -L $dest && $(readlink -- "$dest") == "$detail" ]] || printf '%s: symlink to %s expected, found %s\n' "$path" "$detail" "$(readlink -- "$dest" 2>/dev/null || echo 'none')" >>"$work/entry-bad"
        ;;
      fifo)
        [[ -p $dest ]] || printf '%s: a FIFO was expected\n' "$path" >>"$work/entry-bad"
        ;;
      chardev | blockdev)
        want='character special file'
        [[ $type == blockdev ]] && want='block special file'
        if [[ $(stat -c %F -- "$dest" 2>/dev/null || true) != "$want" ]]; then
          printf '%s: a %s was expected\n' "$path" "$want" >>"$work/entry-bad"
        else
          have=$(printf '%d:%d' "0x$(stat -c %t -- "$dest")" "0x$(stat -c %T -- "$dest")")
          [[ $have == "$detail" ]] || printf '%s: device %s expected, found %s\n' "$path" "$detail" "$have" >>"$work/entry-bad"
        fi
        ;;
      sparse)
        size=${detail#size=}
        if [[ ! -f $dest || $(stat -c %s -- "$dest") != "$size" ]]; then
          printf '%s: a file of %s bytes was expected\n' "$path" "$size" >>"$work/entry-bad"
        else
          alloc=$(($(stat -c %b -- "$dest") * $(stat -c %B -- "$dest")))
          ((alloc * 2 < size)) || printf '%s: expected a sparse file, found %d of %d bytes allocated\n' "$path" "$alloc" "$size" >>"$work/entry-bad"
        fi
        ;;
      *) printf '%s: no check for entry type %s\n' "$path" "$type" >>"$work/entry-bad" ;;
    esac
    if [[ -e /mnt/$disk/$path || -L /mnt/$disk/$path ]]; then printf '%s: still on /mnt/%s\n' "$path" "$disk" >>"$work/entry-bad"; fi
  done <"$work/entries"
  report "special entries that did not arrive intact" "$work/entry-bad" || bad=1
  if ((bad)); then return 1; fi
  printf '%d files of %s are on %s with the recorded sha256 and %d symlinks, FIFOs, device nodes and sparse files arrived intact; nothing is left on the data disks\n' "$nfiles" "$share" "$cache" "$nentries"
}

case $cmd in
  files) [[ $# -eq 3 ]] || die "usage: files <expected> <root> <pool-slots>"; cmd_files "$@" ;;
  owners) [[ $# -eq 3 ]] || die "usage: owners <expected> <root> <pool-slots>"; cmd_owners "$@" ;;
  relocated) [[ $# -eq 4 ]] || die "usage: relocated <expected> <cache> <share> <pool-slots>"; cmd_relocated "$@" ;;
  *) die "unknown command '$cmd'" ;;
esac
