import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "@/App";
import type { components } from "@/lib/api/client";
import { ToolsMigratePage } from "@/routes/tools-migrate";
import {
  migrationPending,
  migrationPendingTemplates,
  type Migration,
  type MigrationReportRow,
  type MigrationTemplates,
} from "@/test/migration-pending";

type Job = components["schemas"]["Job"];
type ApiResult = { data?: unknown; error?: unknown; response: { ok: boolean } };
type PostOptions = { body?: unknown; bodySerializer?: () => FormData; params?: { path?: { jobId?: string } } };

const mockGet = vi.fn();
const mockPost = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  },
}));

const DOCS_BEFORE_YOU_START = "https://hoserva.dev/migrating-from-unraid/before-you-start";
const DOCS_THE_MIGRATION = "https://hoserva.dev/migrating-from-unraid/the-migration";

const ok = (data: unknown): ApiResult => ({ data, response: { ok: true } });
const refused = (message: string, code = "invalid_zip"): ApiResult => ({
  error: { code, message },
  response: { ok: false },
});

const SCAN_JOB: Job = {
  id: "00000000-0000-0000-0000-0000000000a1",
  type: "migration_scan",
  class: "topology",
  status: "running",
  progress: 40,
  resumable: false,
  cancellable: true,
  createdAt: "2026-10-03T10:00:00Z",
};

interface Backend {
  migration: ApiResult | (() => ApiResult);
  templates: ApiResult;
  jobs: ApiResult;
  report: ApiResult;
}

let backend: Backend;

function withReport(rows: (current: MigrationReportRow[]) => MigrationReportRow[], unverified = false): Migration {
  const report = migrationPending.report;
  if (!report) {
    throw new Error("the migration-pending fixture has a report");
  }
  return { ...migrationPending, report: { ...report, unverifiedLayout: unverified, rows: rows(report.rows) } };
}

function templatesWith(extra: MigrationTemplates["templates"]): MigrationTemplates {
  return { ...migrationPendingTemplates, templates: [...migrationPendingTemplates.templates, ...extra] };
}

const TEMPLATE_ONLY = {
  name: "old-app",
  file: "my-old-app.xml",
  class: "template_only" as const,
  counted: false,
  status: "clean" as const,
  warningCount: 0,
};

function installBackend(): void {
  mockGet.mockImplementation((path: string) => {
    switch (path) {
      case "/migrate":
        return Promise.resolve(typeof backend.migration === "function" ? backend.migration() : backend.migration);
      case "/migrate/templates":
        return Promise.resolve(backend.templates);
      case "/migrate/report":
        return Promise.resolve(backend.report);
      case "/jobs":
        return Promise.resolve(backend.jobs);
      case "/setup/status":
        return Promise.resolve(ok({ adminExists: true }));
      case "/auth/session":
        return Promise.resolve(
          ok({ id: "00000000-0000-0000-0000-000000000001", username: "admin", role: "admin", totpEnrolled: false }),
        );
      default:
        return Promise.resolve({ data: null, response: { ok: false } });
    }
  });
}

function renderPage(): ReturnType<typeof render> {
  return render(
    <MemoryRouter>
      <ToolsMigratePage />
    </MemoryRouter>,
  );
}

async function selectOption(comboboxName: string, optionName: string): Promise<void> {
  fireEvent.click(screen.getByRole("combobox", { name: comboboxName }));
  const option = await screen.findByRole("option", { name: optionName });
  fireEvent.pointerDown(option, { pointerType: "mouse" });
  fireEvent.pointerUp(option, { pointerType: "mouse" });
  fireEvent.click(option);
}

function chooseZip(file: File): void {
  const input = document.getElementById("migration-flash-backup");
  if (!(input instanceof HTMLInputElement)) {
    throw new Error("the Flash Backup upload field is not shown");
  }
  fireEvent.change(input, { target: { files: [file] } });
}

const noMigration: Migration = { phase: "none", flashDevices: [], zipOnly: false };

describe("the migration workspace", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    backend = {
      migration: ok(migrationPending),
      templates: ok(migrationPendingTemplates),
      jobs: ok({ jobs: [] }),
      report: ok("# Hoserva migration scan report\n"),
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

  describe("resuming from the server's phase", () => {
    it("opens on Review when a scan has already finished, with the disk, boot and share rows read-only", async () => {
      renderPage();

      expect(await screen.findByText("Step 2 of 4")).toBeInTheDocument();
      expect(screen.getByText("disk2")).toBeInTheDocument();
      expect(screen.getByText("Unraid boots from a USB stick. Keep the stick: it is the rollback.")).toBeInTheDocument();
      expect(screen.getByText("Boot device", { selector: "h3" })).toBeInTheDocument();
      expect(screen.getAllByText(/allocation High-water/)).toHaveLength(1);
      expect(screen.getAllByText("Blocked").length).toBeGreaterThan(0);
      expect(screen.queryByRole("combobox", { name: "Role" })).not.toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Import" })).toBeDisabled();
    });

    it("opens on Scan with the progress of a scan that is still running, and does not offer a second one", async () => {
      backend.migration = ok({ ...noMigration, phase: "scanning" });
      backend.jobs = ok({ jobs: [SCAN_JOB] });
      renderPage();

      expect(await screen.findByText("Unraid migration scan")).toBeInTheDocument();
      expect(screen.getByText("Step 1 of 4")).toBeInTheDocument();
      expect(document.getElementById("migration-flash-backup")).toBeNull();
      expect(screen.getByRole("button", { name: /Start scan/ })).toBeDisabled();
      const jobsCall = mockGet.mock.calls.find((call) => call[0] === "/jobs");
      expect(jobsCall?.[1]).toMatchObject({ params: { query: { class: "topology" } } });
    });

    it("cancels the running scan through the jobs API and says so when the cancel fails", async () => {
      backend.migration = ok({ ...noMigration, phase: "scanning" });
      backend.jobs = ok({ jobs: [SCAN_JOB] });
      mockPost.mockResolvedValue(refused("the job already finished", "job_not_cancellable"));
      renderPage();

      fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));

      expect(await screen.findByText("the job already finished")).toBeInTheDocument();
      const [path, options] = mockPost.mock.calls[0] as [string, PostOptions];
      expect(path).toBe("/jobs/{jobId}/cancel");
      expect(options.params?.path?.jobId).toBe(SCAN_JOB.id);
    });

    it("stays on the Scan step, showing its report, when a scan finishes while the page is open", async () => {
      let calls = 0;
      backend.migration = () => {
        calls += 1;
        // The first answer starts polling, which asks again at once.
        return calls <= 2 ? ok({ ...noMigration, phase: "scanning" }) : ok(migrationPending);
      };
      backend.jobs = ok({ jobs: [SCAN_JOB] });
      renderPage();

      expect(await screen.findByText("Unraid migration scan")).toBeInTheDocument();

      expect(await screen.findByText("Blocks the migration", {}, { timeout: 6000 })).toBeInTheDocument();
      expect(screen.getByText("Step 1 of 4")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Review" })).toBeEnabled();
    }, 10000);

    it("shows why the latest scan failed and offers the form again", async () => {
      backend.migration = ok({ ...noMigration, phase: "scan_failed", scanError: "the Flash Backup could not be read" });
      renderPage();

      expect(await screen.findByText("The scan did not finish")).toBeInTheDocument();
      expect(screen.getByText("the Flash Backup could not be read")).toBeInTheDocument();
      expect(document.getElementById("migration-flash-backup")).not.toBeNull();
    });
  });

  describe("the Scan step", () => {
    it("shows the report grouped by outcome, with the verdict and a download button", async () => {
      renderPage();
      fireEvent.click(await screen.findByRole("button", { name: "Back" }));

      expect(await screen.findByText("Step 1 of 4")).toBeInTheDocument();
      expect(screen.getByText("No-go")).toBeInTheDocument();
      expect(screen.getByText("Blocks the migration")).toBeInTheDocument();
      expect(screen.getByText("Warnings and flagged items")).toBeInTheDocument();
      expect(screen.getByText("Passed or for information")).toBeInTheDocument();
      expect(screen.getByText(/disk3 is not adopted: its read-only xfs check failed/)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Download the report" })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Review" })).toBeEnabled();
    });

    it("downloads the report document the API serves", async () => {
      const blobs: Blob[] = [];
      const names: string[] = [];
      URL.createObjectURL = vi.fn((blob: Blob | MediaSource) => {
        blobs.push(blob as Blob);
        return "blob:report";
      });
      URL.revokeObjectURL = vi.fn();
      vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
        names.push(this.download);
      });
      renderPage();
      fireEvent.click(await screen.findByRole("button", { name: "Back" }));

      fireEvent.click(await screen.findByRole("button", { name: "Download the report" }));

      await waitFor(() => expect(names).toEqual(["hoserva-migration-report.md"]));
      expect(blobs[0]?.size).toBe("# Hoserva migration scan report\n".length);
      expect(mockGet.mock.calls.some((call) => call[0] === "/migrate/report")).toBe(true);
    });

    it("says when the report could not be downloaded instead of saving an empty file", async () => {
      URL.createObjectURL = vi.fn(() => "blob:report");
      backend.report = refused("no scan has finished yet", "no_migration_report");
      renderPage();
      fireEvent.click(await screen.findByRole("button", { name: "Back" }));

      fireEvent.click(await screen.findByRole("button", { name: "Download the report" }));

      expect(await screen.findByText("no scan has finished yet")).toBeInTheDocument();
      expect(URL.createObjectURL).not.toHaveBeenCalled();
    });

    it("sends the chosen zip to the scan and shows the API's message when it is refused", async () => {
      backend.migration = ok(noMigration);
      mockPost.mockResolvedValue(refused("the file is not a zip archive"));
      renderPage();
      expect(await screen.findByText("Step 1 of 4")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Start scan" })).toBeDisabled();
      const zip = new File(["not a zip"], "flash.zip");

      chooseZip(zip);
      expect(screen.getByRole("button", { name: "Start scan" })).toBeEnabled();
      fireEvent.click(screen.getByRole("button", { name: "Start scan" }));

      expect(await screen.findByText("the file is not a zip archive")).toBeInTheDocument();
      const [path, options] = mockPost.mock.calls[0] as [string, PostOptions];
      expect(path).toBe("/migrate/scan");
      expect(options.bodySerializer?.().get("file")).toBe(zip);
      expect(screen.getByText("Step 1 of 4")).toBeInTheDocument();
      expect(screen.queryByText("Blocks the migration")).not.toBeInTheDocument();
      expect(document.getElementById("migration-flash-backup")).not.toBeNull();
    });

    it("shows the message of an unknown layout the same way", async () => {
      backend.migration = ok(noMigration);
      mockPost.mockResolvedValue(
        refused("this Unraid version or flash layout is not one Hoserva has been verified against", "unsupported_layout"),
      );
      renderPage();
      await screen.findByText("Step 1 of 4");

      chooseZip(new File(["zip"], "flash.zip"));
      fireEvent.click(screen.getByRole("button", { name: "Start scan" }));

      expect(await screen.findByText(/not one Hoserva has been verified against/)).toBeInTheDocument();
    });

    it("shows a request that fails outright as an error, not as a started scan", async () => {
      backend.migration = ok(noMigration);
      mockPost.mockRejectedValue(new Error("network down"));
      renderPage();
      await screen.findByText("Step 1 of 4");

      chooseZip(new File(["zip"], "flash.zip"));
      fireEvent.click(screen.getByRole("button", { name: "Start scan" }));

      expect(await screen.findByText("network down")).toBeInTheDocument();
      expect(screen.queryByText("Unraid migration scan")).not.toBeInTheDocument();
    });

    it("offers the attached stick from the session's flash devices and scans the device it names", async () => {
      backend.migration = ok({ ...noMigration, flashDevices: migrationPending.flashDevices });
      mockPost.mockResolvedValue(refused("the stick could not be read", "flash_device_unreadable"));
      renderPage();

      fireEvent.click(await screen.findByRole("radio", { name: /Unraid USB stick/ }));
      expect(screen.getByRole("combobox", { name: "USB stick" })).toBeInTheDocument();
      expect(document.getElementById("migration-flash-backup")).toBeNull();
      fireEvent.click(screen.getByRole("button", { name: "Start scan" }));

      expect(await screen.findByText("the stick could not be read")).toBeInTheDocument();
      const [path, options] = mockPost.mock.calls[0] as [string, PostOptions];
      expect(path).toBe("/migrate/scan/device");
      expect(options.body).toEqual({ device: "/dev/sdu" });
    });

    it("does not offer the stick when Unraid booted internally, and says the zip is the only source", async () => {
      backend.migration = ok({ ...noMigration, zipOnly: true });
      renderPage();

      expect(await screen.findByText("Only the Flash Backup zip can be used")).toBeInTheDocument();
      expect(screen.queryByRole("radio", { name: /Unraid USB stick/ })).not.toBeInTheDocument();
      expect(document.getElementById("migration-flash-backup")).not.toBeNull();
    });

    it("says no stick is attached when none is, without offering one", async () => {
      backend.migration = ok(noMigration);
      renderPage();

      expect(await screen.findByText(/No Unraid USB stick is attached/)).toBeInTheDocument();
      expect(screen.queryByRole("radio", { name: /Unraid USB stick/ })).not.toBeInTheDocument();
    });

    it("keeps the unverified-layout warning on the Scan step and on Review", async () => {
      backend.migration = ok(withReport((rows) => rows, true));
      renderPage();

      expect(await screen.findByText("This Unraid version or flash layout has not been verified")).toBeInTheDocument();
      fireEvent.click(screen.getByRole("button", { name: "Back" }));
      await screen.findByText("Blocks the migration");
      expect(screen.getByText("This Unraid version or flash layout has not been verified")).toBeInTheDocument();
    });

    it("shows no unverified-layout warning for a report that was not made under the override", async () => {
      renderPage();

      await screen.findByText("Step 2 of 4");
      expect(screen.queryByText("This Unraid version or flash layout has not been verified")).not.toBeInTheDocument();
    });

    it("names Unraid's trademark owner wherever the page names Unraid", async () => {
      renderPage();

      expect(
        await screen.findByText("Unraid is a trademark of Lime Technology, Inc. Hoserva is not affiliated with it."),
      ).toBeInTheDocument();
    });
  });

  describe("the unprotected-window warning", () => {
    it("is a banner with no dismiss control, wording the window and linking to the docs", async () => {
      renderPage();

      const title = await screen.findByText("The unprotected window");
      const banner = title.closest('[role="alert"]');
      expect(banner).not.toBeNull();
      const scoped = within(banner as HTMLElement);
      expect(scoped.getByText(/has no redundancy at all/)).toBeInTheDocument();
      expect(scoped.queryByRole("button")).not.toBeInTheDocument();
      expect(scoped.getByRole("link", { name: "Read before you start" })).toHaveAttribute("href", DOCS_BEFORE_YOU_START);
    });

    it("is not shown before the Review step", async () => {
      backend.migration = ok(noMigration);
      renderPage();

      await screen.findByText("Step 1 of 4");
      expect(screen.queryByText("The unprotected window")).not.toBeInTheDocument();
    });
  });

  describe("the Review step", () => {
    it("lists the templates with warning counts and their class, hiding template-only ones until asked", async () => {
      backend.templates = ok(templatesWith([TEMPLATE_ONLY]));
      renderPage();

      expect(await screen.findByText("gateway")).toBeInTheDocument();
      expect(screen.getByText("photos")).toBeInTheDocument();
      expect(screen.getByText("2 warnings")).toBeInTheDocument();
      expect(screen.getByText("No warnings")).toBeInTheDocument();
      expect(screen.getByText("Running")).toBeInTheDocument();
      expect(screen.getByText("Autostart")).toBeInTheDocument();
      expect(screen.queryByText("old-app")).not.toBeInTheDocument();
    });

    it("filters the templates by class and can show every one of them", async () => {
      backend.templates = ok(templatesWith([TEMPLATE_ONLY]));
      renderPage();
      await screen.findByText("gateway");

      await selectOption("Filter by what the template stands for", "Template only");
      expect(await screen.findByText("old-app")).toBeInTheDocument();
      expect(screen.queryByText("gateway")).not.toBeInTheDocument();

      await selectOption("Filter by what the template stands for", "Autostart");
      expect(await screen.findByText("photos")).toBeInTheDocument();
      expect(screen.queryByText("old-app")).not.toBeInTheDocument();
      expect(screen.queryByText("gateway")).not.toBeInTheDocument();

      await selectOption("Filter by what the template stands for", "All templates");
      expect(await screen.findByText("old-app")).toBeInTheDocument();
      expect(screen.getByText("gateway")).toBeInTheDocument();
    });

    it("shows a Compose Manager project in its own group", async () => {
      renderPage();

      expect(await screen.findByText("Compose Manager projects")).toBeInTheDocument();
      expect(screen.getByText("stack")).toBeInTheDocument();
      expect(screen.getByText("stack-web")).toBeInTheDocument();
    });

    it("shows captured containers without a template and containers made by hand as flagged rows", async () => {
      renderPage();

      expect(await screen.findByText("Containers that need attention")).toBeInTheDocument();
      expect(screen.getByText("dbtool")).toBeInTheDocument();
      expect(screen.getByText("handmade")).toBeInTheDocument();
      expect(screen.getByText(/A dockerMan container with no template whose <Name> matches/)).toBeInTheDocument();
      expect(screen.getByText(/Created by hand \(docker run\)/)).toBeInTheDocument();
    });

    it("warns about a missing container capture with the row's own text, explains it and links to the capture steps, without blocking", async () => {
      const detail = "config/hoserva/ is not in the Flash Backup.";
      backend.migration = ok(withReport((rows) => [...rows, { check: "capture", status: "warn", detail }]));
      backend.templates = ok({
        ...migrationPendingTemplates,
        counts: { ...migrationPendingTemplates.counts, allTemplates: true },
        templates: migrationPendingTemplates.templates.map((template) => ({ ...template, class: "unknown" as const })),
      });
      renderPage();

      const title = await screen.findByText("The container capture needs a look");
      const banner = title.closest('[role="alert"]') as HTMLElement;
      expect(within(banner).getByText(detail)).toBeInTheDocument();
      expect(within(banner).getByText(/Hoserva's prepare script, which you run on the Unraid server/)).toBeInTheDocument();
      expect(await within(banner).findByText(/Every template is listed as unknown/)).toBeInTheDocument();
      expect(within(banner).getByRole("link", { name: "Read the capture steps" })).toHaveAttribute(
        "href",
        DOCS_THE_MIGRATION,
      );
      expect(screen.getByText("Step 2 of 4")).toBeInTheDocument();
      expect(await screen.findByText("gateway")).toBeInTheDocument();
    });

    it("words a stale capture's warning from its own text and does not claim every template is unknown", async () => {
      const detail =
        "The capture was taken 2026-09-30T10:00:00Z, but a template on the flash was saved later (2026-09-30T14:00:00Z), so the capture may be stale.";
      backend.migration = ok(withReport((rows) => [...rows, { check: "capture", status: "warn", detail }]));
      renderPage();

      const title = await screen.findByText("The container capture needs a look");
      const banner = title.closest('[role="alert"]') as HTMLElement;
      expect(within(banner).getByText(detail)).toBeInTheDocument();
      expect(await screen.findByText("gateway")).toBeInTheDocument();
      expect(screen.getByText("Autostart")).toBeInTheDocument();
      expect(within(banner).queryByText(/Every template is listed as unknown/)).not.toBeInTheDocument();
      expect(screen.getByText("Step 2 of 4")).toBeInTheDocument();
    });

    it("shows no capture warning for a capture that was read", async () => {
      backend.migration = ok(
        withReport((rows) => [...rows, { check: "capture", status: "pass", detail: "The capture was read." }]),
      );
      renderPage();

      await screen.findByText("gateway");
      expect(screen.queryByText("The container capture needs a look")).not.toBeInTheDocument();
    });

    it("explains a scan with no matched disks instead of showing an empty table", async () => {
      backend.migration = ok(withReport((rows) => rows.filter((row) => row.check !== "disk_mapping")));
      renderPage();

      expect(await screen.findByText("No disks matched")).toBeInTheDocument();
      expect(screen.getByText(/found on this machine by its serial number or WWN/)).toBeInTheDocument();
    });

    it("does not show the no-match explanation when disks matched", async () => {
      renderPage();

      await screen.findByText("Step 2 of 4");
      expect(screen.queryByText("No disks matched")).not.toBeInTheDocument();
    });

    it("shows the API's error when the template preview cannot be loaded, not an empty list", async () => {
      backend.templates = refused("this report was made before scans converted templates", "no_template_preview");
      renderPage();

      expect(await screen.findByText("this report was made before scans converted templates")).toBeInTheDocument();
      expect(screen.queryByText("No Docker templates were found on the flash.")).not.toBeInTheDocument();
      expect(screen.getByText("disk2")).toBeInTheDocument();
    });

    it("goes back to the Scan step and on to a new scan from Review", async () => {
      renderPage();

      fireEvent.click(await screen.findByRole("button", { name: "Back" }));
      fireEvent.click(await screen.findByRole("button", { name: "Scan again" }));

      expect(await screen.findByText("Step 1 of 4")).toBeInTheDocument();
      expect(document.getElementById("migration-flash-backup")).not.toBeNull();
      expect(screen.getByRole("button", { name: "Start scan" })).toBeDisabled();
    });
  });

  describe("when the session cannot be loaded", () => {
    it("shows the API's error and no wizard, never an empty or no-migration state", async () => {
      backend.migration = refused("database is locked", "internal");
      renderPage();

      expect(await screen.findByText("database is locked")).toBeInTheDocument();
      expect(screen.queryByText(/Step \d of 4/)).not.toBeInTheDocument();
      expect(document.getElementById("migration-flash-backup")).toBeNull();
      expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
    });

    it("shows a request that rejects as an error too", async () => {
      mockGet.mockImplementation((path: string) =>
        path === "/migrate" ? Promise.reject(new Error("network down")) : Promise.resolve(ok({})),
      );
      renderPage();

      expect(await screen.findByText("network down")).toBeInTheDocument();
      expect(screen.queryByText(/Step \d of 4/)).not.toBeInTheDocument();
    });

    it("loads the session again from the retry button", async () => {
      backend.migration = refused("database is locked", "internal");
      renderPage();
      await screen.findByText("database is locked");

      backend.migration = ok(migrationPending);
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));

      expect(await screen.findByText("Step 2 of 4")).toBeInTheDocument();
    });

    it("does not leave the previous scan's report on screen when the reload after starting a scan fails", async () => {
      let calls = 0;
      backend.migration = () => {
        calls += 1;
        return calls === 1 ? ok(migrationPending) : refused("database is locked", "internal");
      };
      mockPost.mockResolvedValue(ok({}));
      renderPage();
      fireEvent.click(await screen.findByRole("button", { name: "Back" }));
      fireEvent.click(await screen.findByRole("button", { name: "Scan again" }));
      chooseZip(new File(["zip"], "flash.zip"));
      fireEvent.click(screen.getByRole("button", { name: "Start scan" }));

      expect(await screen.findByText("database is locked")).toBeInTheDocument();
      expect(mockPost).toHaveBeenCalledTimes(1);
      expect(screen.queryByText("Blocks the migration")).not.toBeInTheDocument();
      expect(screen.queryByText(/Step \d of 4/)).not.toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
    });
  });

  describe("reachability", () => {
    it("is routed at /tools/migrate, not left a placeholder", async () => {
      window.history.replaceState({}, "", "/tools/migrate");
      render(<App />);

      expect(await screen.findByText("Step 2 of 4")).toBeInTheDocument();
      expect(screen.queryByText(/placeholder/i)).not.toBeInTheDocument();
      window.history.replaceState({}, "", "/");
    });
  });
});
