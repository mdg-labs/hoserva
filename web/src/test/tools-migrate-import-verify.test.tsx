import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { components } from "@/lib/api/client";
import { ToolsMigratePage } from "@/routes/tools-migrate";
import {
  migrationImported,
  migrationPending,
  migrationPendingTemplates,
  migrationVerified,
  migrationVerifyFailed,
  parityInit,
  verifyPassed,
  type Migration,
  type ParityInit,
} from "@/test/migration-pending";

type Job = components["schemas"]["Job"];
type ApiResult = { data?: unknown; error?: unknown; response: { ok: boolean } };
type RequestOptions = { body?: unknown; params?: { query?: { class?: string }; path?: { jobId?: string } } };

const mockGet = vi.fn();
const mockPost = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  },
}));

const ok = (data: unknown): ApiResult => ({ data, response: { ok: true } });
const refused = (message: string, code: string): ApiResult => ({ error: { code, message }, response: { ok: false } });

const PHRASE = "ERASE /dev/nvme0n1, /dev/sdb";
const CONFIRM_LABEL = "Type the confirmation phrase";
const T0 = "2026-10-04T10:00:00Z";
const T1 = "2026-10-04T10:00:05Z";

let seq = 0;
function job(type: Job["type"], status: Job["status"], extra: Partial<Job> = {}): Job {
  seq += 1;
  return {
    id: `00000000-0000-0000-0000-${String(seq).padStart(12, "0")}`,
    type,
    class: type === "sync" ? "parity" : "topology",
    status,
    resumable: false,
    cancellable: false,
    createdAt: T0,
    ...extra,
  };
}

const GO_REPORT_MIGRATION: Migration = {
  ...migrationPending,
  report: migrationPending.report ? { ...migrationPending.report, verdict: "go_with_warnings" } : undefined,
};

const SHARES = {
  shares: [
    {
      name: "backup",
      path: "/mnt/user/backup",
      cacheMode: "array-only",
      migration: { notes: [] },
    },
    {
      name: "media",
      path: "/mnt/user/media",
      cacheMode: "array-only",
      migration: { targetCacheMode: "cache-then-move", notes: ["High-water allocation was mapped to Balance across disks."] },
    },
    { name: "hand-made", path: "/mnt/user/hand-made", cacheMode: "array-only" },
  ],
};

const USERS = {
  users: [
    { id: "00000000-0000-0000-0000-0000000000f1", username: "admin", role: "admin", hasCredential: true },
    { id: "00000000-0000-0000-0000-0000000000f2", username: "alice", role: "share-only", hasCredential: false },
    { id: "00000000-0000-0000-0000-0000000000f3", username: "bob", role: "share-only", hasCredential: true },
  ],
};

interface Backend {
  migration: ApiResult;
  jobs: ApiResult;
  syncJobs: ApiResult;
  shares: ApiResult;
  users: ApiResult;
  disks: ApiResult;
}

let backend: Backend;

function jobsOf(...jobs: Job[]): ApiResult {
  return ok({ jobs });
}

function installBackend(): void {
  mockGet.mockImplementation((path: string, options?: RequestOptions) => {
    switch (path) {
      case "/migrate":
        return Promise.resolve(backend.migration);
      case "/migrate/templates":
        return Promise.resolve(ok(migrationPendingTemplates));
      case "/jobs":
        return Promise.resolve(options?.params?.query?.class === "parity" ? backend.syncJobs : backend.jobs);
      case "/shares":
        return Promise.resolve(backend.shares);
      case "/users":
        return Promise.resolve(backend.users);
      case "/disks":
        return Promise.resolve(backend.disks);
      default:
        return Promise.resolve({ data: null, response: { ok: false } });
    }
  });
}

function renderPage(migration: Migration, ...jobs: Job[]): ReturnType<typeof render> {
  backend.migration = ok(migration);
  backend.jobs = jobsOf(...jobs);
  return render(
    <MemoryRouter>
      <ToolsMigratePage />
    </MemoryRouter>,
  );
}

function button(name: string | RegExp): HTMLElement {
  return screen.getByRole("button", { name });
}

function postedTo(path: string): [string, RequestOptions] | undefined {
  return mockPost.mock.calls.find((call) => call[0] === path) as [string, RequestOptions] | undefined;
}

function typeConfirmation(value: string): void {
  fireEvent.change(screen.getByLabelText(CONFIRM_LABEL), { target: { value } });
}

async function selectOption(comboboxName: string, optionName: string): Promise<void> {
  fireEvent.click(await screen.findByRole("combobox", { name: comboboxName }));
  const option = await screen.findByRole("option", { name: optionName });
  fireEvent.pointerDown(option, { pointerType: "mouse" });
  fireEvent.pointerUp(option, { pointerType: "mouse" });
  fireEvent.click(option);
}

function withParityInit(patch: Partial<ParityInit>, migration: Migration = migrationVerified): Migration {
  return { ...migration, parityInit: { ...parityInit, ...patch } };
}

describe("the migration Import and Verify steps", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    seq = 0;
    backend = {
      migration: ok(migrationPending),
      jobs: jobsOf(),
      syncJobs: jobsOf(),
      shares: ok(SHARES),
      users: ok(USERS),
      disks: ok({ disks: [] }),
    };
    installBackend();
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      addEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
      matches: false,
      media: query,
      onchange: null,
      removeEventListener: vi.fn(),
    })) as unknown as typeof window.matchMedia;
  });

  describe("starting the import from Review", () => {
    it("offers Import only once the mapping is confirmed, and sends the mapping the user confirmed", async () => {
      renderPage(GO_REPORT_MIGRATION);
      mockPost.mockImplementation((path: string) => {
        if (path === "/migrate/import") {
          const started = job("migration_import", "succeeded");
          backend.migration = ok(migrationImported);
          backend.jobs = jobsOf(started);
          return Promise.resolve(ok(started));
        }
        return Promise.resolve({ data: null, response: { ok: false } });
      });
      expect(await screen.findByText("Step 2 of 4")).toBeInTheDocument();
      expect(button("Import")).toBeDisabled();

      fireEvent.click(screen.getByRole("checkbox", { name: /I have checked each disk's serial number/ }));
      expect(button("Import")).toBeEnabled();
      fireEvent.click(button("Import"));

      expect(await screen.findByText("Step 3 of 4")).toBeInTheDocument();
      expect(postedTo("/migrate/import")?.[1].body).toEqual({
        roles: [
          { role: "parity", wwn: "0x5000c500a1b2c3d4" },
          { role: "data", serial: "EXAMPLE_DISK1" },
          { role: "data", serial: "EXAMPLE_DISK4" },
          { role: "cache", serial: "EXAMPLE_CACHE" },
        ],
        confirm: true,
      });
    });

    it("takes the confirmation back when a role changes", async () => {
      renderPage(GO_REPORT_MIGRATION);
      fireEvent.click(await screen.findByRole("checkbox", { name: /I have checked each disk's serial number/ }));
      expect(button("Import")).toBeEnabled();

      await selectOption("Role for EXAMPLE_DISK4", "Ignore");

      expect(button("Import")).toBeDisabled();
      expect(screen.getByRole("checkbox", { name: /I have checked each disk's serial number/ })).not.toBeChecked();
    });

    it("does not offer the import for a report that blocks the migration", async () => {
      renderPage(migrationPending);

      fireEvent.click(await screen.findByRole("checkbox", { name: /I have checked each disk's serial number/ }));

      expect(button("Import")).toBeDisabled();
      expect(screen.getByText(/so the import is not offered/)).toBeInTheDocument();
    });

    it("shows the API's refusal as returned and stays on Review", async () => {
      renderPage(GO_REPORT_MIGRATION);
      mockPost.mockResolvedValue(refused("an array already exists and is not an Unraid import", "array_exists"));
      fireEvent.click(await screen.findByRole("checkbox", { name: /I have checked each disk's serial number/ }));

      fireEvent.click(button("Import"));

      expect(await screen.findByText("an array already exists and is not an Unraid import")).toBeInTheDocument();
      expect(screen.getByText("The import was refused")).toBeInTheDocument();
      expect(screen.getByText("Step 2 of 4")).toBeInTheDocument();
      expect(button("Import")).toBeEnabled();
    });

    it("names a cache on the boot disk by a spare partition the user picks, and not before", async () => {
      const shared: Migration = {
        ...GO_REPORT_MIGRATION,
        report: GO_REPORT_MIGRATION.report?.review
          ? {
              ...GO_REPORT_MIGRATION.report,
              review: {
                ...GO_REPORT_MIGRATION.report.review,
                disks: GO_REPORT_MIGRATION.report.review.disks.map((disk) =>
                  disk.slot === "pool cache" ? { ...disk, hostBoot: true } : disk,
                ),
              },
            }
          : undefined,
      };
      backend.disks = ok({
        disks: [
          {
            device: "/dev/nvme0n1",
            sizeBytes: 536870912000,
            boot: true,
            cachePartitions: [
              {
                device: "/dev/nvme0n1p3",
                sizeBytes: 107374182400,
                byIdName: "nvme-EXAMPLE_CACHE-part3",
                partUuid: "5b3d9e0a-03",
                reason: "spare_boot_partition",
              },
            ],
          },
        ],
      });
      renderPage(shared);
      mockPost.mockResolvedValue(refused("stop here", "invalid_import_roles"));
      fireEvent.click(await screen.findByRole("checkbox", { name: /I have checked each disk's serial number/ }));

      expect(button("Import")).toBeDisabled();
      expect(screen.getByText(/Choose its spare partition to continue/)).toBeInTheDocument();

      await selectOption("Spare partition", "/dev/nvme0n1p3 (100 GiB)");
      fireEvent.click(screen.getByRole("checkbox", { name: /I have checked each disk's serial number/ }));
      fireEvent.click(button("Import"));

      await screen.findByText("stop here");
      const body = postedTo("/migrate/import")?.[1].body as { roles: Array<Record<string, string>> };
      expect(body.roles.at(-1)).toEqual({ role: "cache", byId: "nvme-EXAMPLE_CACHE-part3", partUuid: "5b3d9e0a-03" });
    });
  });

  describe("the Import step", () => {
    it("resumes on the progress of an import that is still running, and offers nothing else", async () => {
      renderPage(migrationPending, job("migration_import", "running", { progress: 40, cancellable: true }));

      expect(await screen.findByText("Step 3 of 4")).toBeInTheDocument();
      expect(screen.getByText("Unraid disk adoption")).toBeInTheDocument();
      expect(screen.getByText(/Adopting the data disks/)).toBeInTheDocument();
      expect(button(/Continue to verification/)).toBeDisabled();
      expect(screen.queryByRole("button", { name: /parity/i })).not.toBeInTheDocument();
      expect(screen.queryByLabelText(CONFIRM_LABEL)).not.toBeInTheDocument();
    });

    it("cancels the running import through the jobs API and shows the API's message when that fails", async () => {
      const running = job("migration_import", "running", { progress: 10, cancellable: true });
      renderPage(migrationPending, running);
      mockPost.mockResolvedValue(refused("the job already finished", "job_not_cancellable"));

      fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));

      expect(await screen.findByText("the job already finished")).toBeInTheDocument();
      const [path, options] = mockPost.mock.calls[0] as [string, RequestOptions];
      expect(path).toBe("/jobs/{jobId}/cancel");
      expect(options.params?.path?.jobId).toBe(running.id);
    });

    it("lists the seeded shares and the accounts that still need a password, and links to Users", async () => {
      renderPage(migrationImported, job("migration_import", "succeeded"));

      expect(await screen.findByText("The data disks are adopted")).toBeInTheDocument();
      const shares = within(await screen.findByRole("region", { name: "Shares created" }));
      expect(await shares.findByText("backup")).toBeInTheDocument();
      expect(shares.getByText("media")).toBeInTheDocument();
      expect(shares.getByText("Cache then move once the cache exists")).toBeInTheDocument();
      expect(shares.getByText(/High-water allocation was mapped to Balance across disks/)).toBeInTheDocument();
      expect(shares.queryByText("hand-made")).not.toBeInTheDocument();
      const users = within(screen.getByRole("region", { name: "Accounts that need a password" }));
      expect(await users.findByText("alice has no password yet")).toBeInTheDocument();
      expect(users.queryByText(/bob/)).not.toBeInTheDocument();
      expect(users.queryByText(/admin/)).not.toBeInTheDocument();
      expect(users.getByRole("link", { name: "Set passwords" })).toHaveAttribute("href", "/users");
    });

    it("ends at the verification checkpoint without offering parity", async () => {
      renderPage(migrationImported, job("migration_import", "succeeded"));

      await screen.findByText("The data disks are adopted");
      expect(screen.queryByRole("button", { name: /Initialise parity/ })).not.toBeInTheDocument();
      expect(screen.queryByLabelText(CONFIRM_LABEL)).not.toBeInTheDocument();

      fireEvent.click(button("Continue to verification"));

      expect(await screen.findByText("Step 4 of 4")).toBeInTheDocument();
      expect(button("Run verify")).toBeEnabled();
      expect(screen.queryByRole("button", { name: /Initialise parity/ })).not.toBeInTheDocument();
    });

    it("shows the failed import's message when it ends while the page is open", async () => {
      backend.migration = ok(GO_REPORT_MIGRATION);
      const failed = job("migration_import", "failed", { error: { code: "job_failed", message: "disk3 changed since the request" } });
      renderPage(GO_REPORT_MIGRATION);
      mockPost.mockImplementation(() => {
        backend.jobs = jobsOf(failed);
        return Promise.resolve(ok(failed));
      });
      fireEvent.click(await screen.findByRole("checkbox", { name: /I have checked each disk's serial number/ }));

      fireEvent.click(button("Import"));

      expect(await screen.findByText("The import did not finish")).toBeInTheDocument();
      expect(screen.getByText("disk3 changed since the request")).toBeInTheDocument();
      expect(screen.getByText("Step 3 of 4")).toBeInTheDocument();
      expect(button(/Continue to verification/)).toBeDisabled();
      expect(button("Back")).toBeEnabled();
    });

    it("waits for the session to report the adopted array after the job has finished", async () => {
      renderPage(GO_REPORT_MIGRATION);
      let adopted = false;
      mockGet.mockImplementation((path: string, options?: RequestOptions) => {
        if (path === "/migrate") {
          return Promise.resolve(adopted ? ok(migrationImported) : ok(GO_REPORT_MIGRATION));
        }
        if (path === "/jobs") {
          return Promise.resolve(options?.params?.query?.class === "parity" ? backend.syncJobs : backend.jobs);
        }
        return Promise.resolve(path === "/shares" ? backend.shares : path === "/users" ? backend.users : ok(migrationPendingTemplates));
      });
      mockPost.mockImplementation(() => {
        const started = job("migration_import", "succeeded");
        backend.jobs = jobsOf(started);
        return Promise.resolve(ok(started));
      });
      fireEvent.click(await screen.findByRole("checkbox", { name: /I have checked each disk's serial number/ }));

      fireEvent.click(button("Import"));

      expect(await screen.findByText("Waiting for the import to start.")).toBeInTheDocument();
      expect(button(/Continue to verification/)).toBeDisabled();
      adopted = true;
      expect(await screen.findByText("The data disks are adopted", {}, { timeout: 6000 })).toBeInTheDocument();
      expect(button(/Continue to verification/)).toBeEnabled();
    }, 10000);

    it("says so when the shares cannot be loaded instead of showing none", async () => {
      backend.shares = refused("the shares are unavailable", "internal");
      renderPage(migrationImported, job("migration_import", "succeeded"));

      expect(await screen.findByText("the shares are unavailable")).toBeInTheDocument();
      expect(screen.queryByText("The import created no new shares.")).not.toBeInTheDocument();
    });

    it("says so when the jobs cannot be loaded", async () => {
      backend.migration = ok(migrationImported);
      backend.jobs = refused("the jobs are unavailable", "internal");
      render(
        <MemoryRouter>
          <ToolsMigratePage />
        </MemoryRouter>,
      );

      expect(await screen.findByText("the jobs are unavailable")).toBeInTheDocument();
    });
  });

  describe("the Verify step", () => {
    it("runs the verify, and shows a failed result by the API's message and never as success", async () => {
      renderPage(migrationImported, job("migration_import", "succeeded"));
      mockPost.mockImplementation((path: string) => {
        if (path === "/migrate/verify") {
          const failed = job("migration_verify", "failed", { createdAt: T1 });
          backend.migration = ok(migrationVerifyFailed);
          backend.jobs = jobsOf(failed, job("migration_import", "succeeded"));
          return Promise.resolve(ok(failed));
        }
        return Promise.resolve({ data: null, response: { ok: false } });
      });
      fireEvent.click(await screen.findByRole("button", { name: "Continue to verification" }));

      fireEvent.click(await screen.findByRole("button", { name: "Run verify" }));

      expect(await screen.findByText("Stop and investigate")).toBeInTheDocument();
      expect(postedTo("/migrate/verify")).toBeDefined();
    });

    it("shows a failed verify with its mismatches, an error banner and only Re-run verify", async () => {
      renderPage(migrationVerifyFailed, job("migration_verify", "failed"));

      expect(await screen.findByText("Step 4 of 4")).toBeInTheDocument();
      expect(screen.getByText("Stop and investigate")).toBeInTheDocument();
      expect(screen.getByText(/Do not initialise parity/)).toBeInTheDocument();
      expect(screen.queryByText("Every disk and share matches what the scan recorded.")).not.toBeInTheDocument();
      expect(screen.getAllByText("Mismatch").length).toBeGreaterThanOrEqual(2);
      const disk1 = within(screen.getAllByText("Disk disk1")[0]?.closest("tr") as HTMLElement);
      expect(disk1.getByText("Mismatch")).toBeInTheDocument();
      expect(disk1.getByText("41,872 expected, 41,871 found")).toBeInTheDocument();
      const disk3 = within(screen.getAllByText("Disk disk3")[0]?.closest("tr") as HTMLElement);
      expect(disk3.getByText("Matches")).toBeInTheDocument();
      expect(screen.getAllByText("backup/2025/photos-0412.tar").length).toBeGreaterThanOrEqual(1);
      expect(screen.getAllByText("media/movies/Example (2019)/example.mkv").length).toBeGreaterThanOrEqual(1);
      expect(screen.getAllByText("documents/taxes/2024/return.pdf").length).toBeGreaterThanOrEqual(1);
      expect(screen.getAllByText("1 file or link is missing:").length).toBeGreaterThanOrEqual(1);
      expect(button("Re-run verify")).toBeEnabled();
      expect(screen.queryByRole("button", { name: /Initialise parity/ })).not.toBeInTheDocument();
      expect(screen.queryByLabelText(CONFIRM_LABEL)).not.toBeInTheDocument();
    });

    it("does not offer parity for a result the server calls passed while a scope still differs", async () => {
      const [first, ...rest] = verifyPassed.disks ?? [];
      const contradicting: Migration = {
        ...migrationVerified,
        verify: { ...verifyPassed, disks: [{ ...first, passed: false }, ...rest] },
      };
      renderPage(contradicting);

      expect(await screen.findByText("Stop and investigate")).toBeInTheDocument();
      expect(screen.queryByLabelText(CONFIRM_LABEL)).not.toBeInTheDocument();
      expect(button("Re-run verify")).toBeEnabled();
    });

    it("shows why a verify could not finish and offers it again", async () => {
      renderPage(
        { ...migrationVerifyFailed, verify: { status: "failed", startedAt: T0, duplicates: 0, error: "the pool was not mounted read-only" } },
      );

      expect(await screen.findByText(/The verify could not finish: the pool was not mounted read-only/)).toBeInTheDocument();
      expect(button("Re-run verify")).toBeEnabled();
    });

    it("shows the API's message when the verify cannot be started and does not advance", async () => {
      renderPage(migrationImported, job("migration_import", "succeeded"));
      mockPost.mockResolvedValue(refused("no import is pending its point of no return", "no_import_pending"));
      fireEvent.click(await screen.findByRole("button", { name: "Continue to verification" }));

      fireEvent.click(await screen.findByRole("button", { name: "Run verify" }));

      expect(await screen.findByText("no import is pending its point of no return")).toBeInTheDocument();
      expect(screen.getByText("Step 4 of 4")).toBeInTheDocument();
      expect(screen.queryByLabelText(CONFIRM_LABEL)).not.toBeInTheDocument();
    });

    it("resumes on the progress of a verify that is still running", async () => {
      renderPage({ ...migrationImported, phase: "verifying" }, job("migration_verify", "running", { progress: 55, cancellable: true }));

      expect(await screen.findByText("Unraid migration verify")).toBeInTheDocument();
      expect(screen.getByText("Step 4 of 4")).toBeInTheDocument();
      expect(button(/Run verify|Re-run verify/)).toBeDisabled();
    });

    it("lists the paths a verify counted on more than one disk without calling them mismatches", async () => {
      renderPage({
        ...migrationVerified,
        verify: { ...verifyPassed, duplicates: 2, duplicateSample: [{ path: "media/a.mkv", disks: ["disk1", "disk4"] }] },
      });

      expect(await screen.findByText(/2 paths exist on more than one disk/)).toBeInTheDocument();
      expect(screen.getByText("media/a.mkv (on disk1, disk4)")).toBeInTheDocument();
      expect(screen.getByText("Every disk and share matches what the scan recorded.")).toBeInTheDocument();
    });
  });

  describe("initialising parity", () => {
    it("offers it after a passing verify behind the server's confirmation string, with the devices it erases", async () => {
      renderPage(migrationVerified);

      expect(await screen.findByText("Every disk and share matches what the scan recorded.")).toBeInTheDocument();
      expect(screen.queryByText("Mismatch")).not.toBeInTheDocument();
      expect(screen.getAllByText("Matches").length).toBeGreaterThan(1);
      expect(screen.getByText(`Type exactly: ${PHRASE}`)).toBeInTheDocument();
      expect(screen.getByText(/Erases all of \/dev\/sdb \(EXAMPLE_PARITY, 8.00 TiB\) and formats it as the array's parity disk\./)).toBeInTheDocument();
      expect(screen.getByText(/Erases all of \/dev\/nvme0n1 \(EXAMPLE_CACHE, 500 GiB\) and formats it as the array's cache\./)).toBeInTheDocument();
      expect(screen.getByText(parityInit.unprotectedWindow)).toBeInTheDocument();
      for (const line of parityInit.rollback) {
        expect(screen.getByText(line)).toBeInTheDocument();
      }
      expect(screen.getByText("The unprotected window")).toBeInTheDocument();
      expect(button("Initialise parity")).toBeDisabled();
    });

    it("enables the button only when the server's string is typed exactly, and submits that string", async () => {
      renderPage(migrationVerified);
      mockPost.mockResolvedValue(refused("stop", "confirmation_required"));
      await screen.findByLabelText(CONFIRM_LABEL);

      typeConfirmation("ERASE /dev/sdb");
      expect(button("Initialise parity")).toBeDisabled();

      typeConfirmation(PHRASE);
      expect(button("Initialise parity")).toBeEnabled();
      fireEvent.click(button("Initialise parity"));

      await waitFor(() => expect(postedTo("/migrate/initialize-parity")).toBeDefined());
      expect(postedTo("/migrate/initialize-parity")?.[1].body).toEqual({ confirmation: PHRASE });
    });

    it("names a spare partition as only the partition that is erased", async () => {
      renderPage(
        withParityInit({
          confirmation: "ERASE /dev/nvme0n1p3, /dev/sdb",
          erases: [
            { role: "cache", device: "/dev/nvme0n1p3", size: 107374182400, partition: true },
            ...parityInit.erases.filter((erase) => erase.role === "parity"),
          ],
        }),
      );

      expect(
        await screen.findByText(/Erases only the spare partition \/dev\/nvme0n1p3 \(100 GiB\) and formats it as the array's cache\. The rest of that disk is left alone\./),
      ).toBeInTheDocument();
    });

    it("shows the API's refusal as returned, keeps the typed string and does not advance", async () => {
      renderPage(migrationVerified);
      mockPost.mockResolvedValue(refused("the latest verify of the adopted array did not pass", "verify_required"));
      await screen.findByLabelText(CONFIRM_LABEL);
      typeConfirmation(PHRASE);

      fireEvent.click(button("Initialise parity"));

      expect(await screen.findByText("the latest verify of the adopted array did not pass")).toBeInTheDocument();
      expect(screen.getByText("The initialisation was refused")).toBeInTheDocument();
      expect(screen.getByText("Step 4 of 4")).toBeInTheDocument();
      expect(screen.getByLabelText(CONFIRM_LABEL)).toHaveValue(PHRASE);
      expect(screen.queryByText("The array is set up")).not.toBeInTheDocument();
      expect(screen.getByText("The unprotected window")).toBeInTheDocument();
      expect(button("Initialise parity")).toBeEnabled();
    });

    it("shows a wrong-confirmation refusal the same way", async () => {
      renderPage(migrationVerified);
      mockPost.mockResolvedValue(refused("this operation requires an explicit confirmation", "confirmation_required"));
      await screen.findByLabelText(CONFIRM_LABEL);
      typeConfirmation(PHRASE);

      fireEvent.click(button("Initialise parity"));

      expect(await screen.findByText("this operation requires an explicit confirmation")).toBeInTheDocument();
    });

    it("shows the progress of the first sync after the point of no return, and keeps the unprotected-window banner", async () => {
      renderPage(migrationVerified);
      mockPost.mockImplementation(() => {
        const parity = job("migration_parity", "succeeded", { createdAt: T0 });
        backend.migration = ok(migrationPending);
        backend.jobs = jobsOf(parity);
        backend.syncJobs = jobsOf(job("sync", "running", { createdAt: T1, progress: 12, cancellable: true }));
        return Promise.resolve(ok(parity));
      });
      await screen.findByLabelText(CONFIRM_LABEL);
      typeConfirmation(PHRASE);

      fireEvent.click(button("Initialise parity"));

      expect(await screen.findByText("The array is set up")).toBeInTheDocument();
      expect(await screen.findByText("Parity sync")).toBeInTheDocument();
      expect(screen.getByText("The unprotected window")).toBeInTheDocument();
      expect(button("Done")).toBeDisabled();
      expect(screen.queryByLabelText(CONFIRM_LABEL)).not.toBeInTheDocument();
      const parityCall = mockGet.mock.calls.find((call) => (call[1] as RequestOptions).params?.query?.class === "parity");
      expect(parityCall).toBeDefined();
    });

    it("resumes on the progress of an initialisation that is still running", async () => {
      renderPage(migrationVerified, job("migration_parity", "running", { progress: 30, cancellable: false }));

      expect(await screen.findByText("Unraid parity initialisation")).toBeInTheDocument();
      expect(screen.getByText(/Formatting the former parity and cache disks/)).toBeInTheDocument();
      expect(screen.queryByLabelText(CONFIRM_LABEL)).not.toBeInTheDocument();
      expect(screen.getByText("The unprotected window")).toBeInTheDocument();
    });

    it("keeps the unprotected-window banner while the first sync is not yet queued", async () => {
      renderPage(migrationPending, job("migration_parity", "succeeded"));

      expect(await screen.findByText("Waiting for the first parity sync to be queued.")).toBeInTheDocument();
      expect(screen.getByText("The unprotected window")).toBeInTheDocument();
      expect(button("Done")).toBeDisabled();
    });

    it("ends the unprotected window only when the first sync has succeeded", async () => {
      backend.syncJobs = jobsOf(job("sync", "succeeded", { createdAt: T1 }));
      renderPage(migrationPending, job("migration_parity", "succeeded"));

      expect(await screen.findByText("The first sync is complete")).toBeInTheDocument();
      expect(screen.queryByText("The unprotected window")).not.toBeInTheDocument();
      expect(button("Done")).toBeEnabled();
    });

    it("keeps the banner and says so when the first sync failed", async () => {
      backend.syncJobs = jobsOf(
        job("sync", "failed", { createdAt: T1, error: { code: "job_failed", message: "snapraid exited 2" } }),
      );
      renderPage(migrationPending, job("migration_parity", "succeeded"));

      expect(await screen.findByText("The first sync did not complete")).toBeInTheDocument();
      expect(screen.getByText("snapraid exited 2")).toBeInTheDocument();
      expect(screen.getByText("The unprotected window")).toBeInTheDocument();
      expect(screen.getByRole("link", { name: "Open Parity" })).toHaveAttribute("href", "/storage/parity");
      expect(button("Done")).toBeDisabled();
    });

    it("ends the window when a later sync succeeds while the page stays open after the first one failed", async () => {
      backend.syncJobs = jobsOf(
        job("sync", "failed", { createdAt: T1, error: { code: "job_failed", message: "snapraid exited 2" } }),
      );
      renderPage(migrationPending, job("migration_parity", "succeeded"));
      expect(await screen.findByText("The first sync did not complete")).toBeInTheDocument();
      expect(button("Done")).toBeDisabled();

      backend.syncJobs = jobsOf(
        job("sync", "succeeded", { createdAt: "2026-10-04T12:00:00Z" }),
        job("sync", "failed", { createdAt: T1, error: { code: "job_failed", message: "snapraid exited 2" } }),
      );

      expect(await screen.findByText("The first sync is complete", {}, { timeout: 6000 })).toBeInTheDocument();
      expect(screen.queryByText("The unprotected window")).not.toBeInTheDocument();
      expect(button("Done")).toBeEnabled();
    });

    it("ends the window at a later sync that succeeded after the first one failed", async () => {
      backend.syncJobs = jobsOf(
        job("sync", "succeeded", { createdAt: "2026-10-04T12:00:00Z" }),
        job("sync", "failed", {
          createdAt: T1,
          error: { code: "job_failed", message: "snapraid exited 2" },
        }),
      );
      renderPage(migrationPending, job("migration_parity", "succeeded"));

      expect(await screen.findByText("The first sync is complete")).toBeInTheDocument();
      expect(screen.queryByText("The unprotected window")).not.toBeInTheDocument();
      expect(screen.queryByText("The first sync did not complete")).not.toBeInTheDocument();
      expect(button("Done")).toBeEnabled();
    });

    it("does not take an older sync for the first one", async () => {
      backend.syncJobs = jobsOf(job("sync", "succeeded", { createdAt: "2026-10-01T10:00:00Z" }));
      renderPage(migrationPending, job("migration_parity", "succeeded"));

      expect(await screen.findByText("Waiting for the first parity sync to be queued.")).toBeInTheDocument();
      expect(screen.getByText("The unprotected window")).toBeInTheDocument();
    });

    it("says so when the first sync cannot be loaded", async () => {
      backend.syncJobs = refused("the parity jobs are unavailable", "internal");
      renderPage(migrationPending, job("migration_parity", "succeeded"));

      expect(await screen.findByText("the parity jobs are unavailable")).toBeInTheDocument();
      expect(screen.getByText("The unprotected window")).toBeInTheDocument();
      expect(button("Done")).toBeDisabled();
    });

    it("shows a failed initialisation's message and lets the confirmation be typed again", async () => {
      renderPage(
        migrationVerified,
        job("migration_parity", "failed", { error: { code: "job_failed", message: "disk /dev/sdb changed" } }),
      );

      expect(await screen.findByText("The initialisation did not finish")).toBeInTheDocument();
      expect(screen.getByText("disk /dev/sdb changed")).toBeInTheDocument();
      typeConfirmation(PHRASE);
      expect(button("Initialise parity")).toBeEnabled();
    });

    it("finishes an initialisation that stopped after the disks were formatted, with the server's string", async () => {
      const finishing: ParityInit = {
        finishing: true,
        confirmation: "FINISH PARITY INITIALISATION",
        erases: [],
        unprotectedWindow: parityInit.unprotectedWindow,
        rollback: parityInit.rollback,
      };
      renderPage({ ...migrationImported, phase: "initializing", parityInit: finishing });
      mockPost.mockResolvedValue(refused("stop", "confirmation_required"));

      expect(await screen.findByText("Finish the parity initialisation")).toBeInTheDocument();
      expect(screen.queryByText(/Erases/)).not.toBeInTheDocument();
      expect(button("Finish initialisation")).toBeDisabled();
      typeConfirmation("FINISH PARITY INITIALISATION");
      fireEvent.click(button("Finish initialisation"));

      await waitFor(() => expect(postedTo("/migrate/initialize-parity")).toBeDefined());
      expect(postedTo("/migrate/initialize-parity")?.[1].body).toEqual({ confirmation: "FINISH PARITY INITIALISATION" });
    });

    it("shows why the server does not offer it now and gives no confirmation to type", async () => {
      renderPage(withParityInit({ confirmation: undefined, erases: [], problem: "disk /dev/sdb is not the disk that was imported" }));

      expect(await screen.findByText("Parity cannot be initialised now")).toBeInTheDocument();
      expect(screen.getByText("disk /dev/sdb is not the disk that was imported")).toBeInTheDocument();
      expect(screen.queryByLabelText(CONFIRM_LABEL)).not.toBeInTheDocument();
      expect(button("Initialise parity")).toBeDisabled();
    });
  });
});
