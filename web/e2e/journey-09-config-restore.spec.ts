import { execFile } from "node:child_process";
import { readFileSync, statSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import {
  expect,
  type APIRequestContext,
  type Locator,
  type Page,
  test,
} from "@playwright/test";

// Journey 9 (doc 06 §4): "Export config -> wipe VM -> fresh install ->
// import config -> system matches", driven through the pages (doc 03 §1,
// §8.5; doc 10 §1). Phase 1 signs in as the L3 admin, records the system
// and downloads the config archive from /settings/backup. The harness then
// replaces the VM's OS disk and installs Hoserva fresh (scripts/vm/
// reinstall-os.sh, run by HOSERVA_E2E_REINSTALL_CMD), keeping the array
// disks. Phase 2 onboards the fresh box with a different, temporary admin,
// uploads the archive on /settings/backup, confirms the previewed disk
// mapping and the typed phrase, reads the restore report, signs in as the
// L3 admin again and compares.
//
// This is its own Playwright project with no stored session (the spec signs
// in and onboards itself) and is not part of the generic `chromium` run: a
// reinstall would leave the specs after it on a box with a stale session.
// Every failure on a page is asserted as that page's error banner, never
// left to a timeout.
const reinstallCommand = process.env.HOSERVA_E2E_REINSTALL_CMD ?? "";
const adminUsername = process.env.HOSERVA_E2E_USERNAME ?? "hoserva-l3";
const adminPassword = process.env.HOSERVA_E2E_PASSWORD ?? "hoserva-l3-suite-password";

const TEMP_ADMIN = { username: "journey9-temp-admin", password: "journey9-temporary-password" };
const JOURNEY_PASSPHRASE = "journey9-backup-passphrase";
const JOURNEY_SHARE = "journey9share";
const JOURNEY_USER = "journey9viewer";
const JOURNEY_CHAIN = { startTime: "04:19", weeklyScrubDay: 5 };
const JOURNEY_JOB = { id: "container_update_check", enabled: true, frequency: "weekly", time: "04:43" };
const TIMEZONES = ["Pacific/Auckland", "America/Halifax"];

const REINSTALL_TIMEOUT_MS = 20 * 60_000;
const RESTORE_TIMEOUT_MS = 120_000;
const POOL_TIMEOUT_MS = 90_000;

const catalogPath = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  "../src/lib/i18n/locales/en.json",
);
const catalog = JSON.parse(readFileSync(catalogPath, "utf-8")) as Record<string, unknown>;

function catalogString(key: string, values: Record<string, string | number> = {}): string {
  const value = key.split(".").reduce<unknown>((node, part) => {
    if (node !== null && typeof node === "object" && part in (node as Record<string, unknown>)) {
      return (node as Record<string, unknown>)[part];
    }
    return undefined;
  }, catalog);
  if (typeof value !== "string") {
    throw new Error(`journey-09: no i18n catalog string at "${key}"`);
  }
  return value.replace(/\{\{(\w+)\}\}/g, (_match, name: string) => {
    if (!(name in values)) {
      throw new Error(`journey-09: no value for {{${name}}} in "${key}"`);
    }
    return String(values[name]);
  });
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

const execFileAsync = promisify(execFile);

type Json = Record<string, unknown>;

async function api(request: APIRequestContext, method: "get" | "post" | "put", apiPath: string, data?: Json): Promise<Json> {
  const response = await request[method](`/api/v1${apiPath}`, data === undefined ? undefined : { data });
  expect(response.ok(), `${method.toUpperCase()} ${apiPath} returned ${response.status()}: ${await response.text()}`).toBeTruthy();
  return (await response.json()) as Json;
}

function pick(row: Json, keys: string[]): Json {
  return Object.fromEntries(
    keys.map((key) => {
      if (!(key in row)) {
        throw new Error(`journey-09: response is missing "${key}": ${JSON.stringify(row)}`);
      }
      return [key, row[key]];
    }),
  );
}

function sortBy(rows: Json[], ...keys: string[]): Json[] {
  return [...rows].sort((a, b) => keys.map((key) => String(a[key])).join("\0").localeCompare(keys.map((key) => String(b[key])).join("\0")));
}

// What a correct restore must reproduce. Next-run times, last-login times
// and usage figures change with time or with the restore itself and are left
// out, as in scripts/vm/baremetal-state.py.
async function captureState(request: APIRequestContext): Promise<Json> {
  const shares = (await api(request, "get", "/shares")).shares as Json[];
  const users = (await api(request, "get", "/users")).users as Json[];
  const schedules = (await api(request, "get", "/settings/schedules")) as { chain: Json; otherJobs: Json[]; conflicts: Json[] };
  const pool = (await api(request, "get", "/pool")).disks as Json[];
  const general = await api(request, "get", "/settings/general");
  return {
    shares: sortBy(shares.map((row) => pick(row, ["name", "path", "cacheMode", "createPolicy", "smb", "nfs"])), "name"),
    users: sortBy(users.map((row) => pick(row, ["id", "username", "role", "hasCredential"])), "username"),
    schedules: {
      chain: {
        ...pick(schedules.chain, ["startTime", "weeklyScrubDay"]),
        steps: (schedules.chain.steps as Json[]).map((row) => pick(row, ["id", "enabled"])),
      },
      otherJobs: sortBy(schedules.otherJobs.map((row) => pick(row, ["id", "enabled", "frequency", "time"])), "id"),
      conflicts: sortBy(schedules.conflicts.map((row) => pick(row, ["jobA", "jobB"])), "jobA", "jobB"),
    },
    poolDisks: sortBy(pool.map((row) => pick(row, ["role", "mountPoint"])), "role", "mountPoint"),
    settings: { hostname: general.hostname ?? "", timezone: general.timezone ?? "" },
  };
}

function arrayDiskCount(state: Json, role: string): number {
  return (state.poolDisks as Json[]).filter((disk) => disk.role === role).length;
}

// expectOutcome waits for either the success or the failure element and, when
// a failure banner is showing, fails with its text.
async function expectOutcome(success: Locator, failure: Locator, what: string, timeout = 30_000): Promise<void> {
  await expect(success.or(failure).first(), `${what}: neither the expected result nor an error banner appeared`).toBeVisible({ timeout });
  if ((await failure.count()) > 0) {
    throw new Error(`${what} failed, the page shows: ${(await failure.allInnerTexts()).join(" | ")}`);
  }
}

async function signInThroughUi(page: Page, username: string, password: string): Promise<void> {
  await page.goto("/login");
  await page.getByLabel(catalogString("login.fields.username"), { exact: true }).fill(username);
  await page.getByLabel(catalogString("login.fields.password"), { exact: true }).fill(password);
  await page.getByRole("button", { name: catalogString("login.submit"), exact: true }).click();
  await expectOutcome(
    page.getByRole("link", { name: catalogString("nav.jobs"), exact: true }),
    // Only the login form's own banner means the sign-in failed; the page loaded
    // after it can carry alerts of its own, such as the dashboard's failed-job banner.
    page.locator("form").getByRole("alert"),
    `signing in as ${username}`,
  );
}

function restoreCard(page: Page): Locator {
  return page.locator('[data-slot="card"]').filter({ hasText: catalogString("settings.backup.restore.description") });
}

// Banners inside the restore card that are not one of its informational
// notes: a refusal from the preview (blockers[]), a failed request, or an
// unmatched-disks warning.
function restoreErrorBanners(card: Locator): Locator {
  const informational = [
    catalogString("settings.backup.restore.notes.sessions_replaced"),
    catalogString("settings.backup.restore.notes.array_state_kept"),
    catalogString("settings.backup.restore.notes.host_files_replaced"),
    catalogString("settings.backup.restore.preview.schemaDiffers"),
    catalogString("settings.backup.restore.report.signedOutTitle"),
  ];
  return card.getByRole("alert").filter({ hasNotText: new RegExp(informational.map(escapeRegExp).join("|")) });
}

async function previewArchive(card: Locator, archive: string, passphrase = ""): Promise<void> {
  await card.getByLabel(catalogString("settings.backup.restore.archive"), { exact: true }).setInputFiles(archive);
  if (passphrase !== "") {
    await card.getByLabel(catalogString("settings.backup.restore.passphrase"), { exact: true }).fill(passphrase);
  }
  await card.getByRole("button", { name: catalogString("settings.backup.restore.previewAction"), exact: true }).click();
  await expectOutcome(
    card.getByText(catalogString("settings.backup.restore.preview.archiveTitle"), { exact: true }),
    restoreErrorBanners(card),
    "previewing the archive",
  );
}

function tableRow(card: Locator, category: string): Locator {
  return card.getByRole("row").filter({ hasText: catalogString(`settings.backup.restore.categories.${category}`) });
}

async function restoredCounts(card: Locator, category: string): Promise<{ added: number; changed: number; removed: number }> {
  const cells = await tableRow(card, category).getByRole("cell").allInnerTexts();
  expect(cells, `the restore report has no row for "${category}"`).toHaveLength(4);
  return { added: Number(cells[1]), changed: Number(cells[2]), removed: Number(cells[3]) };
}

// setBackupPassphrase sets the backup passphrase through /settings/backup, so
// the archive exported afterwards carries secrets.age and identity.age; a
// failed save is asserted as the overlay's own error banner.
async function setBackupPassphrase(page: Page, passphrase: string): Promise<void> {
  await page.goto("/settings/backup");
  await expect(
    page.getByText(catalogString("settings.backup.config.passphraseNotSet"), { exact: true }),
    "expected the source box to have no backup passphrase before the journey sets one",
  ).toBeVisible();
  await page.getByRole("button", { name: catalogString("settings.backup.config.setPassphrase"), exact: true }).click();
  const dialog = page.getByRole("dialog", { name: catalogString("settings.backup.passphrase.setTitle") });
  await dialog.getByLabel(catalogString("settings.backup.passphrase.passphrase"), { exact: true }).fill(passphrase);
  await dialog.getByLabel(catalogString("settings.backup.passphrase.confirm"), { exact: true }).fill(passphrase);
  await dialog.getByRole("button", { name: catalogString("settings.backup.passphrase.save"), exact: true }).click();
  await expectOutcome(
    page.getByText(catalogString("settings.backup.config.passphraseSet"), { exact: true }),
    dialog.getByRole("alert").filter({ hasText: catalogString("settings.backup.passphrase.saveFailed") }),
    "setting the backup passphrase",
  );
}

// exportConfigArchive downloads the config archive from /settings/backup and
// saves it at destination; a failed download is asserted as the page's own
// error banner rather than left to a timeout.
async function exportConfigArchive(page: Page, destination: string): Promise<string> {
  await page.goto("/settings/backup");
  const downloadFailed = page
    .getByRole("alert")
    .filter({ hasText: catalogString("settings.backup.config.downloadFailed") })
    .first();
  const downloadEvent = page.waitForEvent("download", { timeout: 60_000 });
  await page.getByRole("button", { name: catalogString("settings.backup.config.download"), exact: true }).click();
  const exported = await Promise.race([
    downloadEvent.then((value) => ({ kind: "download" as const, value }), () => ({ kind: "timeout" as const })),
    downloadFailed.waitFor({ timeout: 60_000 }).then(
      () => ({ kind: "banner" as const }),
      () => ({ kind: "timeout" as const }),
    ),
  ]);
  if (exported.kind === "banner") {
    throw new Error(`downloading the config backup failed, the page shows: ${await downloadFailed.innerText()}`);
  }
  if (exported.kind === "timeout") {
    throw new Error("downloading the config backup neither produced a file nor showed an error banner within 60s");
  }
  const download = exported.value;
  expect(download.suggestedFilename()).toMatch(/^hoserva-config-.*\.tar\.zst$/);
  await download.saveAs(destination);
  expect(statSync(destination).size, "the downloaded config archive is empty").toBeGreaterThan(0);
  return destination;
}

test.skip(reinstallCommand === "", "HOSERVA_E2E_REINSTALL_CMD is not set: journey 9 needs the L3 harness to replace the VM's OS disk");

test("config export, fresh install and restore through the UI", async ({ browser, baseURL, page, request }, testInfo) => {
  const browserTimezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const journeyTimezone = TIMEZONES.find((zone) => zone !== browserTimezone) ?? TIMEZONES[0];

  // Phase 1: the L3 admin's box.
  await signInThroughUi(page, adminUsername, adminPassword);
  const poolBefore = await api(page.request, "get", "/pool");
  expect(
    poolBefore.mounted,
    "expected the L3 harness's array setup to have left the pool mounted before the export",
  ).toBe(true);

  const existingShares = ((await api(page.request, "get", "/shares")).shares as Json[]).map((share) => share.name);
  if (!existingShares.includes(JOURNEY_SHARE)) {
    await api(page.request, "post", "/shares", { name: JOURNEY_SHARE, cacheMode: "array-only" });
  }
  const existingUsers = ((await api(page.request, "get", "/users")).users as Json[]).map((user) => user.username);
  if (!existingUsers.includes(JOURNEY_USER)) {
    await api(page.request, "post", "/users", { username: JOURNEY_USER, role: "viewer" });
  }
  await api(page.request, "put", "/settings/schedules/chain", JOURNEY_CHAIN);
  await api(page.request, "put", `/settings/schedules/jobs/${JOURNEY_JOB.id}`, {
    enabled: JOURNEY_JOB.enabled,
    frequency: JOURNEY_JOB.frequency,
    time: JOURNEY_JOB.time,
  });
  await api(page.request, "put", "/settings/general", { timezone: journeyTimezone });

  const before = await captureState(page.request);
  expect(
    JSON.stringify(before),
    "the recorded state must hold what this journey created, or comparing against it proves nothing",
  ).toContain(JOURNEY_SHARE);
  expect(JSON.stringify(before)).toContain(JOURNEY_USER);
  expect(JSON.stringify(before)).toContain(JOURNEY_CHAIN.startTime);
  expect(JSON.stringify(before)).toContain(JOURNEY_JOB.time);
  expect((before.settings as Json).timezone).toBe(journeyTimezone);

  await setBackupPassphrase(page, JOURNEY_PASSPHRASE);
  const archivePath = await exportConfigArchive(page, testInfo.outputPath("config-export.tar.zst"));

  // The harness replaces the OS disk and installs Hoserva fresh.
  await page.context().close();
  let reinstallLog: string;
  let reinstallFailure: Error | null = null;
  try {
    const { stdout, stderr } = await execFileAsync(reinstallCommand, [], {
      timeout: REINSTALL_TIMEOUT_MS,
      maxBuffer: 64 * 1024 * 1024,
    });
    reinstallLog = `${stdout}\n${stderr}`;
  } catch (err) {
    const failure = err as { stdout?: string; stderr?: string; message: string };
    reinstallLog = `${failure.stdout ?? ""}\n${failure.stderr ?? ""}`;
    reinstallFailure = new Error(`HOSERVA_E2E_REINSTALL_CMD (${reinstallCommand}) failed: ${failure.message}`, { cause: err });
  }
  await testInfo.attach("reinstall-os.log", { body: reinstallLog, contentType: "text/plain" });
  if (reinstallFailure !== null) {
    const tail = reinstallLog.trim().split("\n").slice(-20).join("\n");
    throw new Error(`${reinstallFailure.message}\n${tail}`, { cause: reinstallFailure });
  }

  // Phase 2: the fresh box, onboarded with a different admin.
  const context = await browser.newContext({ baseURL, ignoreHTTPSErrors: true });
  const fresh = await context.newPage();
  try {
    const usernameField = fresh.getByLabel(catalogString("welcome.fields.username"), { exact: true });
    await expect(async () => {
      await fresh.goto("/");
      await expect(usernameField).toBeVisible({ timeout: 5_000 });
    }, "the fresh install did not show the onboarding wizard").toPass({ timeout: 120_000 });

    const next = fresh.getByRole("button", { name: catalogString("wizard.next"), exact: true });
    await usernameField.fill(TEMP_ADMIN.username);
    await fresh.getByLabel(catalogString("welcome.fields.password"), { exact: true }).fill(TEMP_ADMIN.password);
    await next.click();
    await expectOutcome(
      fresh.getByText(catalogString("welcome.steps.doctor.title"), { exact: true }),
      fresh.getByRole("alert"),
      "creating the temporary admin",
    );
    await expect(next, "the system check step does not let onboarding continue").toBeEnabled();
    await next.click();
    await expectOutcome(
      fresh.getByText(catalogString("welcome.steps.basics.title"), { exact: true }),
      fresh.getByRole("alert"),
      "the system check step",
    );
    await next.click();
    await expectOutcome(
      fresh.getByText(catalogString("welcome.steps.path.title"), { exact: true }),
      fresh.getByRole("alert"),
      "the basics step",
    );
    await fresh.getByRole("button", { name: catalogString("welcome.finish"), exact: true }).click();
    await expect(fresh).toHaveURL(/\/storage\/setup/);

    // The fresh install's own archive: after the restore it describes a
    // different array than the one the box then has, which the preview must
    // refuse (asserted at the end).
    const freshArchivePath = await exportConfigArchive(fresh, testInfo.outputPath("fresh-install-export.tar.zst"));
    const card = restoreCard(fresh);
    await expect(card).toBeVisible();

    // A file that is not an archive shows the page's error banner.
    const garbage = testInfo.outputPath("not-an-archive.tar.zst");
    writeFileSync(garbage, "not a tar.zst archive");
    await card.getByLabel(catalogString("settings.backup.restore.archive"), { exact: true }).setInputFiles(garbage);
    await card.getByRole("button", { name: catalogString("settings.backup.restore.previewAction"), exact: true }).click();
    await expect(
      card.getByRole("alert").filter({ hasText: catalogString("settings.backup.restore.errors.invalid_archive") }),
      "an unreadable archive must show the preview's error banner",
    ).toBeVisible();

    // The box has nothing to seal (no apps, no secret-bearing destinations), so
    // the archive carries identity.age but no secrets.age and the preview, which
    // reports only secrets.age, reads "no secrets section". identity.age is
    // checked by the restore report's nothingLeftOut and the restored box's
    // passphraseSet.
    await previewArchive(card, archivePath, JOURNEY_PASSPHRASE);
    await expect(
      card.getByText(catalogString("settings.backup.restore.secrets.statuses.none"), { exact: true }),
      "an archive of a box with nothing to seal has no secrets section",
    ).toBeVisible();

    for (const share of before.shares as Json[]) {
      await expect(
        card.getByText(
          catalogString("settings.backup.restore.preview.changeItem", {
            kind: catalogString("settings.backup.restore.kinds.share"),
            name: String(share.name),
          }),
          { exact: true },
        ),
      ).toBeVisible();
    }
    const userChange = (name: string): Locator =>
      card.getByText(
        catalogString("settings.backup.restore.preview.changeItem", {
          kind: catalogString("settings.backup.restore.kinds.user"),
          name,
        }),
        { exact: true },
      );
    await expect(userChange(adminUsername), "the preview must list the L3 admin as added").toBeVisible();
    await expect(userChange(TEMP_ADMIN.username), "the preview must list the temporary admin as removed").toBeVisible();

    const arrayDisks = arrayDiskCount(before, "data") + arrayDiskCount(before, "parity") + arrayDiskCount(before, "cache");
    await expect(
      card.getByText(catalogString("settings.backup.restore.disks.states.matched"), { exact: true }),
      "every array disk in the archive must be matched against an attached disk",
    ).toHaveCount(arrayDisks);

    const applyAction = card.getByRole("button", { name: catalogString("settings.backup.restore.apply.action"), exact: true });
    await expect(applyAction, "restoring must stay disabled until the disk mapping is confirmed").toBeDisabled();
    await card.getByRole("switch", { name: catalogString("settings.backup.restore.disks.confirm") }).click();
    await expect(applyAction).toBeEnabled();
    await applyAction.click();

    const dialog = fresh.getByRole("dialog", { name: catalogString("settings.backup.restore.apply.title") });
    const confirm = dialog.getByRole("button", { name: catalogString("settings.backup.restore.apply.confirm"), exact: true });
    await expect(confirm).toBeDisabled();
    await dialog.getByLabel(catalogString("typedConfirm.label")).fill(catalogString("settings.backup.restore.apply.phrase"));
    await confirm.click();
    await expectOutcome(
      card.getByText(catalogString("settings.backup.restore.report.signedOutTitle"), { exact: true }),
      dialog.getByRole("alert"),
      "applying the restore",
      RESTORE_TIMEOUT_MS,
    );

    // The restore report: what was restored, and nothing left out.
    await expect(card.getByText(catalogString("settings.backup.restore.report.nothingLeftOut"), { exact: true })).toBeVisible();
    const shares = await restoredCounts(card, "shares");
    expect(shares.added, "every share of the archive is added on a box that had none").toBeGreaterThanOrEqual((before.shares as Json[]).length);
    const accounts = await restoredCounts(card, "accounts");
    expect(accounts.added, "the archive's accounts are added").toBeGreaterThanOrEqual((before.users as Json[]).length);
    expect(accounts.removed, "the temporary onboarding admin is removed").toBeGreaterThanOrEqual(1);
    await restoredCounts(card, "schedules");
    await restoredCounts(card, "system");

    // The archive's sessions replaced the temporary admin's.
    const signedOut = await fresh.request.get("/api/v1/users");
    expect(signedOut.status(), "the temporary admin's session must be gone after the restore").toBe(401);

    await card.getByRole("link", { name: catalogString("settings.backup.restore.report.signIn"), exact: true }).click();
    await signInThroughUi(fresh, adminUsername, adminPassword);

    // The temporary admin no longer exists.
    const tempLogin = await request.post("/api/v1/auth/login", { data: TEMP_ADMIN });
    expect(tempLogin.status(), "the temporary onboarding admin must no longer be able to sign in").toBe(401);

    // The archive's backup passphrase is the restored box's own.
    await fresh.goto("/settings/backup");
    await expect(
      fresh.getByText(catalogString("settings.backup.config.passphraseSet"), { exact: true }),
      "the restored box must have the archive's backup passphrase set",
    ).toBeVisible();

    // The array comes up.
    await expect
      .poll(async () => (await api(fresh.request, "get", "/pool")).mounted, {
        message: `the pool is not mounted within ${POOL_TIMEOUT_MS / 1000}s of the restore`,
        timeout: POOL_TIMEOUT_MS,
        intervals: [2_000],
      })
      .toBe(true);

    const after = await captureState(fresh.request);
    expect(after, "shares, users, schedules, settings and pool disk roles must equal the state before the export").toEqual(before);

    // A refusal from the preview (blockers[]) is an error banner with the
    // server's message and keeps the restore disabled: the fresh install's
    // archive was made by this same installation but before it had an array.
    await fresh.goto("/settings/backup");
    const refusalCard = restoreCard(fresh);
    await refusalCard.getByLabel(catalogString("settings.backup.restore.archive"), { exact: true }).setInputFiles(freshArchivePath);
    await refusalCard.getByRole("button", { name: catalogString("settings.backup.restore.previewAction"), exact: true }).click();
    const blocker = refusalCard
      .getByRole("alert")
      .filter({ hasText: catalogString("settings.backup.restore.blockers.archive_array_mismatch") });
    await expect(
      blocker,
      "previewing an archive that describes another array must show the blocker's error banner",
    ).toBeVisible();
    expect(
      (await blocker.innerText()).trim().length,
      "the blocker must carry the server's message beside its title",
    ).toBeGreaterThan(catalogString("settings.backup.restore.blockers.archive_array_mismatch").length);
    await expect(refusalCard.getByRole("button", { name: catalogString("settings.backup.restore.apply.action"), exact: true })).toBeDisabled();

    await fresh.goto("/");
    await expect(
      fresh.getByText(catalogString("dashboard.capacity.title"), { exact: true }),
      "the dashboard must show the pool, not the no-array state",
    ).toBeVisible();
    await expect(fresh.getByText(catalogString("dashboard.noArray.title"), { exact: true })).toHaveCount(0);
    await expect(
      fresh.getByText(
        catalogString("dashboard.disks.summary", {
          data: arrayDiskCount(before, "data"),
          parity: arrayDiskCount(before, "parity"),
          cache: arrayDiskCount(before, "cache"),
        }),
        { exact: true },
      ),
    ).toBeVisible();
  } finally {
    await context.close();
  }
});
