#!/usr/bin/env bash
# The host's udev-created /dev/loopN nodes aren't visible inside the
# container, so losetup --find has nothing to attach to until these exist.
# Only the nodes are created here (mknod); which ones can actually be opened
# is enforced by the container's device cgroup rule (`b 7:* rmw` in
# docker-compose.dev.yml), not by anything in this script.
set -euo pipefail

for i in $(seq 0 63); do
  [[ -e "/dev/loop$i" ]] || mknod -m 660 "/dev/loop$i" b 7 "$i"
done

exec "$@"
