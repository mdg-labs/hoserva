#!/usr/bin/env bash
# Build every Go package that carries //go:build lab tests on the host
# (`go test -tags lab -c` — compiling touches no device) and run each
# binary inside the caller's own lab container (doc 06 §3, Q45, issue
# #328). The lab image has no Go toolchain, so the binaries live under
# bin/lab-tests/ on the host and are executed via the /src bind mount.
#
# The standing array is a run-once-per-lab-up resource for any test that
# mutates on-disk state (doc 06 §3). This script destroy+create's the
# array before every package so a dirty parity/content history from one
# package cannot make the next look like a regression.
#
# Run only via `make test-lab` / `make test-integration` with
# HOSERVA_LAB_ID set — never as an ad-hoc docker compose exec.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd -- "$HERE/../.." && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

# Host-side: lab_require_id wants /lab/$id, which only exists inside the
# container. Replicate the id check alone and keep LAB as the host path
# for messaging; the container still sees /lab/$HOSERVA_LAB_ID.
: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID — parallel labs must never share loop devices or paths}"
lab_id_valid "$HOSERVA_LAB_ID" \
  || die "invalid HOSERVA_LAB_ID '$HOSERVA_LAB_ID': must match ^[a-zA-Z0-9][a-zA-Z0-9_.-]*\$ and must not contain '..'"

cd -- "$ROOT"

GO="${GO:-go}"
OUT_DIR="${ROOT}/bin/lab-tests/${HOSERVA_LAB_ID}"
mkdir -p -- "$OUT_DIR"

compose_cmd=(docker compose -f docker-compose.dev.yml)
if [[ -n "${LAB_COMPOSE_EXTRA:-}" ]]; then
  compose_cmd+=(-f "$LAB_COMPOSE_EXTRA")
fi
compose_cmd+=(-p "hoserva-lab-$HOSERVA_LAB_ID")

lab_exec() {
  "${compose_cmd[@]}" exec -T lab "$@"
}

# Refuse to run against a missing or stopped container — mistaking a
# host-side go test for an in-lab run would skip every device assertion.
# Do not swallow compose failures (e.g. no docker socket): that would look
# identical to "no container" and hide a broken harness.
cid="$("${compose_cmd[@]}" ps -aq lab)" \
  || die "docker compose ps failed — is the Docker daemon reachable?"
[[ -n "$cid" ]] || die "lab container hoserva-lab-$HOSERVA_LAB_ID does not exist — run 'make lab-up' first"
running="$("${compose_cmd[@]}" ps --status running -q lab)" \
  || die "docker compose ps failed — is the Docker daemon reachable?"
[[ -n "$running" ]] || die "lab container hoserva-lab-$HOSERVA_LAB_ID is not running — run 'make lab-up' first"

# Packages that contain at least one //go:build lab test file. Sorted for
# stable make/CI output; never a hard-coded list that drifts from the tree.
mapfile -t lab_pkgs < <(
  grep -rl --include='*_test.go' '^//go:build lab$' internal cmd \
    | while IFS= read -r f; do dirname -- "$f"; done \
    | sort -u
)
((${#lab_pkgs[@]} > 0)) || die "no //go:build lab test files under internal/ or cmd/"

echo "run-lab-tests[$HOSERVA_LAB_ID]: compiling ${#lab_pkgs[@]} package(s) with -tags lab"

declare -a binaries=()
declare -a bin_pkgs=()
for pkg_dir in "${lab_pkgs[@]}"; do
  # Flatten import path to a unique binary name (internal/parity →
  # internal-parity.test) so two packages never overwrite each other.
  bin_name="${pkg_dir//\//-}.test"
  bin_path="$OUT_DIR/$bin_name"
  echo "run-lab-tests[$HOSERVA_LAB_ID]: go test -tags lab -c -o bin/lab-tests/${HOSERVA_LAB_ID}/$bin_name ./$pkg_dir"
  # CGO_ENABLED=0 matches the L3 harness (array-sequence-check.sh): the
  # lab image is Debian glibc, but a static binary avoids any host/lab
  # libc skew and never needs a C compiler on the host.
  CGO_ENABLED=0 "$GO" test -tags lab -c -o "$bin_path" "./$pkg_dir"
  [[ -x "$bin_path" ]] || die "expected executable $bin_path after go test -c"
  binaries+=("$bin_path")
  bin_pkgs+=("$pkg_dir")
done

# -test.run '^TestLab' keeps non-lab unit tests (also compiled into the
# binary) off the lab's device path: they already run under make test.
# Helper tests that share the TestLab prefix (e.g. TestLabMover_SIGKILLHelper)
# still match and self-skip when not re-exec'd.
failed=0
for i in "${!binaries[@]}"; do
  bin_path="${binaries[$i]}"
  pkg_dir="${bin_pkgs[$i]}"
  bin_name="$(basename -- "$bin_path")"
  container_bin="/src/bin/lab-tests/$HOSERVA_LAB_ID/$bin_name"

  echo "run-lab-tests[$HOSERVA_LAB_ID]: resetting standing array before ./$pkg_dir"
  lab_exec bash /src/scripts/devenv/destroy-array.sh
  lab_exec bash /src/scripts/devenv/create-array.sh

  echo "run-lab-tests[$HOSERVA_LAB_ID]: running $container_bin (-test.run '^TestLab') for ./$pkg_dir"
  if lab_exec "$container_bin" -test.v -test.count=1 -test.run '^TestLab'; then
    echo "run-lab-tests[$HOSERVA_LAB_ID]: PASS ./$pkg_dir"
  else
    echo "run-lab-tests[$HOSERVA_LAB_ID]: FAIL ./$pkg_dir" >&2
    failed=1
    # Keep going so the report lists every failing package, not just the
    # first — but still exit non-zero so the make target fails.
  fi
done

if ((failed)); then
  die "one or more lab-tagged packages failed — see FAIL lines above"
fi
echo "run-lab-tests[$HOSERVA_LAB_ID]: all ${#binaries[@]} lab package(s) passed"
