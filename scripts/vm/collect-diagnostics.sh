#!/usr/bin/env bash
# nightly-l3.yml's failure-artifact step (#222): pulls the guest's own
# hoservad journal and `hoserva doctor` output into a directory the
# workflow uploads as an artifact, so a red nightly is diagnosable from
# the run alone instead of an agent re-deriving it from the vm-suite
# step's client-side symptom (this issue's own starting point — a bare
# "do request: ... EOF" with no server-side evidence attached).
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

out_dir="${1:?usage: collect-diagnostics.sh <output-dir>}"
mkdir -p -- "$out_dir"

if ! vm_domain_running "$VM_DOMAIN"; then
  echo "collect-diagnostics[$HOSERVA_LAB_ID]: domain '$VM_DOMAIN' is not running — nothing to collect" >&2
  exit 0
fi

vm_ssh 'sudo journalctl -u hoserva --no-pager' >"$out_dir/hoserva.journal.log" 2>&1 || true
vm_ssh 'sudo hoserva doctor --json' >"$out_dir/hoserva-doctor.json" 2>&1 || true

echo "collect-diagnostics[$HOSERVA_LAB_ID]: wrote $out_dir"
