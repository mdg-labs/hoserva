import { join } from "node:path";
import { defineConfig, type ReporterDescription } from "@playwright/test";

// The critical-journeys suite (doc 06 §4): runs against a real, already-
// running Hoserva UI+API — either the L3 VM under `make vm-suite`
// (scripts/vm/run-playwright.sh sets HOSERVA_E2E_BASE_URL to the guest's
// forwarded hoservad TLS port) or `make mock` + `npm run dev` for local
// iteration. This file never starts either itself: the L3 harness and
// `make mock` are the two ways to stand a target up, and duplicating
// either one here as a Playwright `webServer` would be a second,
// divergent copy of logic that already lives in scripts/vm/ and the
// Makefile. The setup project signs in as the L3 admin
// (HOSERVA_E2E_USERNAME / HOSERVA_E2E_PASSWORD, defaulting to the
// credentials run-l3-suite.sh posts to POST /setup/admin) and writes
// storageState for the chromium project. The journey-9 project is selected
// on its own (`--project=journey-9`, scripts/vm/run-playwright.sh).
const baseURL = process.env.HOSERVA_E2E_BASE_URL ?? "http://127.0.0.1:5173";

// With TEST_REPORT_DIR set (doc 06 §7) the run also writes a JUnit file there,
// named for the project run-playwright.sh selected, so the chromium run and
// the journey-9 run of one L3 suite do not overwrite each other.
const reportDir = process.env.TEST_REPORT_DIR;
const junit: ReporterDescription[] = reportDir
  ? [["junit", { outputFile: join(reportDir, `playwright-${process.env.HOSERVA_E2E_REPORT_NAME ?? "e2e"}.xml`) }]]
  : [];

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  retries: 0,
  // nightly-l3.yml uploads web/playwright-report/ as a build artifact —
  // the "list" reporter alone only writes to the terminal, so without
  // "html" that upload always finds nothing.
  reporter: [["list"], ["html", { open: "never" }], ...junit],
  use: {
    baseURL,
    // The L3 guest's hoservad TLS listener carries a locally-issued
    // machine-key certificate (doc 01 §7), never a certificate a browser
    // would trust by default — ignoring HTTPS errors here is scoped to
    // this test-only Playwright context, never the shipped app.
    ignoreHTTPSErrors: true,
    trace: "retain-on-failure",
  },
  projects: [
    {
      name: "setup",
      testMatch: /auth\.setup\.ts/,
    },
    {
      name: "chromium",
      dependencies: ["setup"],
      testIgnore: /journey-09-/,
      use: {
        browserName: "chromium",
        storageState: "e2e/.auth/user.json",
      },
    },
    // Journey 9 replaces the VM's OS disk and onboards the fresh install
    // itself, so it has no stored session and no dependency on the setup
    // project, and nothing else runs against the box it leaves behind.
    // run-l3-suite.sh runs it as its own step after the chromium project;
    // the reinstall and restore together need far more than the default
    // per-test and per-assertion limits.
    {
      name: "journey-9",
      testMatch: /journey-09-.*\.spec\.ts/,
      timeout: 40 * 60_000,
      expect: { timeout: 30_000 },
      use: {
        browserName: "chromium",
      },
    },
  ],
});
