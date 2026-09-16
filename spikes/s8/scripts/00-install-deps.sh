#!/usr/bin/env bash
set -euo pipefail
apt-get update -qq
apt-get install -y --no-install-recommends attr snapraid > /tmp/apt-install.log 2>&1
dpkg-query -W -f '${Package} ${Version}\n' attr snapraid
