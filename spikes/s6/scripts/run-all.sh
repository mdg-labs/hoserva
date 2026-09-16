#!/usr/bin/env bash
# Runs the whole spike, in order, against a freshly built standing lab
# array (`make lab-up`, default geometry — see spikes/s6/README.md).
# Fails loudly on the first non-zero exit, like spike S5's own run-all.sh.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

command -v mergerfs >/dev/null 2>&1 || die "mergerfs is not installed in this lab image — expected in scripts/devenv/Dockerfile already (doc 08)"

OUT=${S6_OUT:-$LAB/s6-results}
mkdir -p -- "$OUT"

{
  echo "mergerfs: $(dpkg-query -W -f '${Version}' mergerfs)"
  echo "xfsprogs: $(dpkg-query -W -f '${Version}' xfsprogs)"
  echo "kernel: $(uname -srvm)"
} | tee "$OUT/versions.log"

for step in \
  01-ordering-shadow-mount.sh \
  02-topology-up.sh \
  03-fallback-mspmfs-epmfs.sh \
  04-nc-branches.sh \
  05-catchall-stray-write.sh \
  06-cost.sh \
  07-failure-kill.sh \
  08-topology-down.sh
do
  s6_log ""
  s6_log "########## $step ##########"
  bash "$HERE/$step"
done

s6_log ""
s6_log "== cleaning up this spike's own working directory ($S6) — not touched by destroy-array.sh =="
rm -rf -- "$S6"

# destroy-array.sh (run by `make lab-destroy`) only ever removes $LAB/img
# and $LAB/mnt — never $OUT — so leaving it in place here would make
# `make lab-destroy`'s own host-side `rm -rf .lab/<id>` fail on these
# root-owned files. Default: remove it, so `make lab-up` -> run-all.sh ->
# `make lab-destroy` needs no manual step in between. Set
# S6_KEEP_RESULTS=1 to keep it around long enough to `docker cp` it out
# (the container stays up until `make lab-destroy` is run) — remove it
# yourself afterward, from inside the container, before destroying the lab.
if [[ "${S6_KEEP_RESULTS:-0}" == "1" ]]; then
  s6_log "S6_KEEP_RESULTS=1: leaving $OUT in place — copy it out (docker cp), then remove it yourself before make lab-destroy"
else
  s6_log "== removing $OUT (this run's own results) so make lab-destroy needs no manual cleanup =="
  rm -rf -- "$OUT"
fi

s6_log "spike S6 run-all.sh: all steps passed"
