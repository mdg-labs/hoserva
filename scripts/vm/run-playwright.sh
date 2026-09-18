#!/usr/bin/env bash
# Runs web/'s Playwright suite (doc 06 §4) against whatever
# HOSERVA_E2E_BASE_URL already points at — this lab's own L3 VM when
# called from run-l3-suite.sh, or any other already-running Hoserva UI
# when run by hand. Never starts a target itself (web/playwright.config.ts's
# own header comment explains why): standing one up is `make vm-up` +
# `make vm-deploy`, or `make mock` + `npm run dev`, not duplicated here.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

: "${HOSERVA_E2E_BASE_URL:?set HOSERVA_E2E_BASE_URL to the UI under test (e.g. https://127.0.0.1:<forwarded-port>)}"

cd "$repo_root/web"
npm ci --no-audit --no-fund
npx playwright install chromium
HOSERVA_E2E_BASE_URL="$HOSERVA_E2E_BASE_URL" npx playwright test
