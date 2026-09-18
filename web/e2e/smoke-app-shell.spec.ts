import { expect, test } from "@playwright/test";

// Not one of doc 06 §4's numbered journeys — a harness smoke test proving
// Playwright itself drives a real browser against a real running Hoserva
// UI, independent of journey-05's expected failure. If this test can't
// pass, the harness itself is broken, not just the feature journey-05
// is waiting on.
test("the app shell loads and its navigation is present", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("link", { name: /jobs/i })).toBeVisible();
});
