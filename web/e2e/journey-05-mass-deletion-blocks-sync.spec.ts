import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { expect, type APIRequestContext, test } from "@playwright/test";

// Journey 5 (doc 06 §4): "Delete many files → diff shows removals → sync
// is blocked → warning is visible." Doc 02 §2 calls the threshold guard
// the single most important safety property in the product; this is its
// UI surface, on /storage/parity (doc 03).
//
// Seeding journey 5's share, its baseline sync and the mass deletion
// itself are scripts/vm/run-l3-suite.sh's own job (its "array setup"
// step and the fixture it prepares ahead of this Playwright run) — they
// write real files through the guest's own pool mount and drive real
// `hoservad` jobs, neither of which a browser-driven test can do. This
// spec only ever drives the real UI and the documented API (D18): it
// runs the diff itself (the guard's own state depends on a fresh one),
// then asserts on what the guard's own UI does with that diff. If the
// harness's array setup or fixture stage failed, the assertions below
// fail for real — there is no skip path.
const catalogPath = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  "../src/lib/i18n/locales/en.json",
);
const catalog = JSON.parse(readFileSync(catalogPath, "utf-8")) as Record<string, unknown>;

function catalogString(key: string): string {
  const value = key.split(".").reduce<unknown>((node, part) => {
    if (node !== null && typeof node === "object" && part in (node as Record<string, unknown>)) {
      return (node as Record<string, unknown>)[part];
    }
    return undefined;
  }, catalog);
  if (typeof value !== "string") {
    throw new Error(`journey-05: no i18n catalog string at "${key}"`);
  }
  return value;
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

interface ParityGuardState {
  wouldBlock: boolean;
  summary?: string;
}

interface ParitySnapshot {
  lastSyncAt?: string;
  guard?: ParityGuardState;
}

interface Job {
  id: string;
  status: string;
}

const TERMINAL_JOB_STATUSES = new Set(["succeeded", "failed", "cancelled", "interrupted"]);

async function waitForJobTerminal(
  request: APIRequestContext,
  jobId: string,
  timeoutMs: number,
): Promise<Job> {
  const deadline = Date.now() + timeoutMs;
  let job: Job = { id: jobId, status: "unknown" };
  while (Date.now() < deadline) {
    const response = await request.get(`/api/v1/jobs/${jobId}`);
    if (response.ok()) {
      job = (await response.json()) as Job;
      if (TERMINAL_JOB_STATUSES.has(job.status)) {
        return job;
      }
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new Error(`job ${jobId} did not reach a terminal status within ${timeoutMs}ms (last seen: ${job.status})`);
}

test("mass deletion blocks the sync", async ({ page }) => {
  const baselineResponse = await page.request.get("/api/v1/parity");
  expect(baselineResponse.ok(), "expected GET /parity to succeed").toBeTruthy();
  const baseline = (await baselineResponse.json()) as ParitySnapshot;
  expect(
    baseline.lastSyncAt,
    "expected a baseline sync already recorded by the L3 harness (run-l3-suite.sh's array setup and journey-5 fixture stage) before this journey runs",
  ).toBeTruthy();
  const baselineLastSyncAt = baseline.lastSyncAt;

  await page.goto("/storage/parity");
  await expect(
    page,
    "expected an authenticated /storage/parity page — a redirect to /login or /welcome means Playwright did not sign in as the L3 admin",
  ).not.toHaveURL(/\/(login|welcome)(?:\/|$|\?)/);

  // Step 1 (doc 06 §4): run a diff and assert the removals group shows
  // the mass deletion the harness already performed.
  await page.getByRole("button", { name: catalogString("parity.diff.runDiff"), exact: true }).click();
  const runDiffDialog = page.getByRole("dialog", { name: catalogString("parity.diff.runDiff") });
  await expect(runDiffDialog).toBeVisible();
  await runDiffDialog
    .getByRole("button", { name: catalogString("parity.diff.runDiffConfirm") })
    .click();
  await expect(runDiffDialog).toBeHidden({ timeout: 60_000 });

  const removedGroup = page.getByRole("button", {
    name: new RegExp(escapeRegExp(catalogString("parity.diff.categories.removed"))),
  });
  await expect(removedGroup).toBeVisible();
  const removedText = await removedGroup.innerText();
  const removedCountMatch = removedText.match(/(\d+)/);
  expect(removedCountMatch, `expected a numeric removed count in "${removedText}"`).not.toBeNull();
  expect(Number(removedCountMatch?.[1])).toBeGreaterThan(0);

  // Step 2: the guard badge and banner reflect the tripped guard.
  await expect(page.getByText(catalogString("parity.guard.blocked"), { exact: true })).toBeVisible();
  await expect(page.getByText(catalogString("parity.guard.bannerTitle"))).toBeVisible();

  const trippedResponse = await page.request.get("/api/v1/parity");
  expect(trippedResponse.ok()).toBeTruthy();
  const tripped = (await trippedResponse.json()) as ParitySnapshot;
  expect(
    tripped.guard?.wouldBlock,
    "expected the threshold guard to report wouldBlock=true after diffing the mass deletion",
  ).toBe(true);

  // Step 3: opening Sync shows the guard's own warning, not the plain
  // sync description — and the dialog is closed again without
  // confirming, so the override is never exercised through the UI.
  await page.getByRole("button", { name: catalogString("parity.actions.sync"), exact: true }).click();
  const syncDialog = page.getByRole("dialog", { name: catalogString("parity.actions.sync") });
  await expect(syncDialog).toBeVisible();
  const plainSyncDescription = catalogString("parity.actions.syncDescription");
  const syncDialogText = await syncDialog.innerText();
  expect(syncDialogText).not.toContain(plainSyncDescription);
  const expectedWarning = tripped.guard?.summary ?? catalogString("parity.guard.bannerDescription");
  await expect(syncDialog).toContainText(expectedWarning);
  await syncDialog.getByRole("button", { name: catalogString("confirm.cancel") }).click();
  await expect(syncDialog).toBeHidden();

  // Step 4 (Q14, doc 02 §2): startSync without the explicit override is
  // refused — the queued job itself fails against the tripped guard,
  // never writing parity. Calling this through the documented API
  // directly (D18) is deliberate: confirming the UI's own Sync dialog
  // would send `confirm: true` and actually perform the override, which
  // this journey must never do.
  const refusedSync = await page.request.post("/api/v1/parity/sync", {
    data: { confirm: false, dryRun: false },
  });
  expect(refusedSync.ok(), "expected startSync to queue a job even though the guard will fail it").toBeTruthy();
  const refusedJob = (await refusedSync.json()) as Job;
  const finishedJob = await waitForJobTerminal(page.request, refusedJob.id, 60_000);
  expect(
    finishedJob.status,
    "expected the unconfirmed sync job to fail against the tripped threshold guard, never succeed",
  ).toBe("failed");

  // Step 5: the refusal is visible in the UI, and parity never advanced
  // past the baseline.
  await page.reload();
  await expect(page.getByText(catalogString("parity.guard.blocked"), { exact: true })).toBeVisible();
  await expect(
    page.locator(`[data-job-id="${refusedJob.id}"]`).getByText("failed", { exact: true }),
  ).toBeVisible();

  const afterResponse = await page.request.get("/api/v1/parity");
  expect(afterResponse.ok()).toBeTruthy();
  const after = (await afterResponse.json()) as ParitySnapshot;
  expect(
    after.lastSyncAt,
    "expected the refused sync to leave parity's last successful sync unchanged from the harness's baseline",
  ).toBe(baselineLastSyncAt);
});
