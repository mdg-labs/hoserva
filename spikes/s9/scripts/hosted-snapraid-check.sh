#!/usr/bin/env bash
# Runs on the CI runner (or a dev host), against the standing lab array
# `make lab-up` already built for $HOSERVA_LAB_ID. snapraid isn't in the
# standing lab image (scripts/devenv/Dockerfile is out of this spike's
# declared scope, and the "install a package for this spike only" pattern
# is the same one spikes/s5 and spikes/s7 already used), so it is apt-get
# installed inside the running container, once, here.
#
# This is the one thing ci.yml's own `lab` job never exercises (doc 08 S9,
# "Hosted CI runners" section) — everything else this script checks
# (loop devices, XFS, the mergerfs pool) is already proven by that job on
# every push.
set -euo pipefail

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"

COMPOSE_DEV="docker compose -f docker-compose.dev.yml -p hoserva-lab-$HOSERVA_LAB_ID"

$COMPOSE_DEV exec -T lab bash /src/spikes/s9/scripts/snapraid-sync-in-container.sh
