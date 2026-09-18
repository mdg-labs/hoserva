import { expect, test } from "@playwright/test";

// Journey 5 (doc 06 §4): "Delete many files → diff shows removals → sync
// is blocked → warning is visible." This is the threshold guard's own UI
// surface — doc 02 §2 calls the guard the single most important safety
// property in the product, and CLAUDE.md's acceptance criterion for this
// issue is that this journey "runs and can never be skipped".
//
// It cannot pass yet: doc 12 §7's App.tsx has no route for a share, a
// diff view or the guard's warning at all — only /jobs is wired to real
// UI, and every other route (including a would-be /shares) renders
// PlaceholderPage ("This section is a placeholder — its page is built in
// a later issue"). Skipping this test until that UI exists would satisfy
// "runs" only by not running at all, which is exactly what CLAUDE.md
// warns against — and doc 09/doc 02 make the guard's own tests
// non-negotiable in the areas that implement it, so this journey stays
// wired to the real app and fails loudly and specifically, naming the
// missing surface, until the mover/threshold-guard UI lands.
test("mass deletion blocks the sync", async ({ page }) => {
  await page.goto("/shares");

  const placeholder = page.getByText("This section is a placeholder", { exact: false });
  await expect(
    placeholder,
    "expected a real shares page with a diff view and the threshold guard's warning, " +
      "but /shares still renders PlaceholderPage — the mover/threshold-guard UI (doc 02 §2, " +
      "doc 09) has not landed yet. This assertion should start failing (placeholder gone) " +
      "once it does; the steps below are what it must then satisfy.",
  ).not.toBeVisible();

  // Not yet reachable — recorded here as the journey's real shape so the
  // test only needs its selectors filled in once the UI exists, not a
  // rewrite:
  //   1. seed a share with many files (via the API, not the UI)
  //   2. delete most of them
  //   3. trigger a diff — assert the removals are shown
  //   4. assert the sync action is disabled/blocked with a visible warning
  //   5. assert an explicit override is required to proceed (Q14/doc 02 §2)
});
