import { defineConfig } from "@playwright/test";

// The critical-journeys suite (doc 06 §4): runs against a real, already-
// running Hoserva UI+API — either the L3 VM under `make vm-suite`
// (scripts/vm/run-playwright.sh sets HOSERVA_E2E_BASE_URL to the guest's
// forwarded hoservad TLS port) or `make mock` + `npm run dev` for local
// iteration. This file never starts either itself: the L3 harness and
// `make mock` are the two ways to stand a target up, and duplicating
// either one here as a Playwright `webServer` would be a second,
// divergent copy of logic that already lives in scripts/vm/ and the
// Makefile.
const baseURL = process.env.HOSERVA_E2E_BASE_URL ?? "http://127.0.0.1:5173";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  retries: 0,
  reporter: [["list"]],
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
      name: "chromium",
      use: { browserName: "chromium" },
    },
  ],
});
