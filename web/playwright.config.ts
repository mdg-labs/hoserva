import { defineConfig } from "@playwright/test";

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
// storageState for the chromium project.
const baseURL = process.env.HOSERVA_E2E_BASE_URL ?? "http://127.0.0.1:5173";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  retries: 0,
  // nightly-l3.yml uploads web/playwright-report/ as a build artifact —
  // the "list" reporter alone only writes to the terminal, so without
  // "html" that upload always finds nothing.
  reporter: [["list"], ["html", { open: "never" }]],
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
      use: {
        browserName: "chromium",
        storageState: "e2e/.auth/user.json",
      },
    },
  ],
});
