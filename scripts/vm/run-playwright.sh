#!/usr/bin/env bash
# Runs web/'s Playwright suite (doc 06 §4) against whatever
# HOSERVA_E2E_BASE_URL already points at — this lab's own L3 VM when
# called from run-l3-suite.sh, or any other already-running Hoserva UI
# when run by hand. Never starts a target itself (web/playwright.config.ts's
# own header comment explains why): standing one up is `make vm-up` +
# `make vm-deploy`, or `make mock` + `npm run dev`, not duplicated here.
#
# run-playwright.sh [project]: with no argument the `chromium` project
# runs (the setup project signs in first). `journey-9` runs journey 9
# alone, which replaces the VM's OS disk through
# HOSERVA_E2E_REINSTALL_CMD and so never shares a run with the other
# specs.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

: "${HOSERVA_E2E_BASE_URL:?set HOSERVA_E2E_BASE_URL to the UI under test (e.g. https://127.0.0.1:<forwarded-port>)}"

project="${1:-chromium}"
case "$project" in
  chromium | journey-9) ;;
  *) echo "run-playwright: unknown project '$project' — valid: chromium, journey-9" >&2; exit 2 ;;
esac

cd "$repo_root/web"
npm ci --no-audit --no-fund
npx playwright install chromium
mkdir -p "$repo_root/web/e2e/.auth"
export HOSERVA_E2E_REPORT_NAME="$project"
export HOSERVA_E2E_USERNAME="${HOSERVA_E2E_USERNAME:-hoserva-l3}"
export HOSERVA_E2E_PASSWORD="${HOSERVA_E2E_PASSWORD:-hoserva-l3-suite-password}"
HOSERVA_E2E_BASE_URL="$HOSERVA_E2E_BASE_URL" \
  HOSERVA_E2E_USERNAME="$HOSERVA_E2E_USERNAME" \
  HOSERVA_E2E_PASSWORD="$HOSERVA_E2E_PASSWORD" \
  npx playwright test --project="$project"
