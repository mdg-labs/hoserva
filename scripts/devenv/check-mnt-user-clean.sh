#!/usr/bin/env bash
# Confirms /mnt/user (pool.CatchAllPath, doc 02 §1) is absent, or already a
# live hoserva-pool mount, before test-lab's own //go:build lab tests run.
# A lab test that stands up the catch-all pool fresh needs that path
# unoccupied; a stray leftover there — a symlink smb-check.sh forgot to
# remove, or anything else — is indistinguishable to that test from a real
# conflicting mount and fails obscurely deep inside pool.Mounter instead of
# here, loudly, before it ever gets that far (issue #363).
#
# Run only inside the lab container, via `make test-integration`, after
# smb-check.sh and before test-lab.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"
lab_require_id

if [[ -e /mnt/user || -L /mnt/user ]]; then
  source=""
  source=$(findmnt -n -o SOURCE /mnt/user 2>/dev/null || true)
  [[ "$source" == "hoserva-pool" ]] \
    || die "/mnt/user already exists before test-lab and is not a live hoserva-pool mount (findmnt SOURCE: '${source:-<none, e.g. a stray symlink>}') — a prior lab step left it behind; see issue #363"
  echo "check-mnt-user-clean: /mnt/user is a live hoserva-pool mount — leaving it as found"
else
  echo "check-mnt-user-clean: /mnt/user is absent — clean for the next lab test"
fi
