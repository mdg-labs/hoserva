// #435, #437: the jobs list, the type filter and the job detail heading must show a
// catalog label for every JobType/JobClass value, and the raw string for a
// value this client does not know yet (a newer daemon than the UI).

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AppShell } from "@/components/patterns/app-shell";
import i18n from "@/lib/i18n";
import { JOB_CLASS_VALUES, JOB_TYPE_VALUES, jobClassLabel, jobTypeLabel } from "@/lib/job-labels";
import { DashboardPage } from "@/routes/dashboard";
import { JobDetailPage } from "@/routes/jobs/detail";
import { JobsPage } from "@/routes/jobs/index";
import { ParityPage } from "@/routes/storage/parity";

const mockGet = vi.fn();
const mockPost = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  },
}));

vi.mock("@/lib/api/auth-context", () => ({
  useAuth: () => ({
    phase: "authenticated",
    user: { id: "1", username: "admin", role: "admin", totpEnrolled: false },
    adminExists: true,
    refresh: vi.fn(),
    acceptSession: vi.fn(),
  }),
}));

afterEach(() => {
  cleanup();
});

describe("job label helpers (#435)", () => {
  it("labels every JobType and JobClass value from the catalog", () => {
    expect(jobTypeLabel("disk_upgrade_parity", i18n.t)).toBe("Upgrade parity disk");
    expect(jobTypeLabel("appdata_restore", i18n.t)).toBe("Appdata restore");
    expect(jobTypeLabel("maintenance_chain", i18n.t)).toBe("maintenance_chain");
    expect(jobClassLabel("array_write", i18n.t)).toBe("Array write");
    expect(jobClassLabel("service", i18n.t)).toBe("Service");
  });

  it("has a catalog string, not the raw value or a missing-key marker, for every JobType", () => {
    expect(JOB_TYPE_VALUES).toHaveLength(31);
    for (const type of JOB_TYPE_VALUES) {
      expect(i18n.exists(`jobs.types.${type}`), type).toBe(true);
      const label = jobTypeLabel(type, i18n.t);
      expect(label, type).not.toBe(type);
      expect(label, type).not.toMatch(/^jobs\./);
    }
  });

  it("has a catalog string, not the raw value or a missing-key marker, for every JobClass", () => {
    expect(JOB_CLASS_VALUES).toHaveLength(5);
    for (const jobClass of JOB_CLASS_VALUES) {
      expect(i18n.exists(`jobs.classes.${jobClass}`), jobClass).toBe(true);
      const label = jobClassLabel(jobClass, i18n.t);
      expect(label, jobClass).not.toBe(jobClass);
      expect(label, jobClass).not.toMatch(/^jobs\./);
    }
  });

  it("falls back to the raw string for a value the client does not know", () => {
    expect(jobTypeLabel("future_job", i18n.t)).toBe("future_job");
    expect(jobTypeLabel("toString", i18n.t)).toBe("toString");
    expect(jobClassLabel("future_class", i18n.t)).toBe("future_class");
  });
});

describe("jobs list labels (#435)", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      addEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
      matches: false,
      media: query,
      onchange: null,
      removeEventListener: vi.fn(),
    })) as unknown as typeof window.matchMedia;
  });

  it("shows the type and class labels, and the raw string for unknown values", async () => {
    mockGet.mockImplementation((path: string) =>
      path === "/jobs"
        ? Promise.resolve({
            data: {
              jobs: [
                { id: "j-1", type: "disk_upgrade_parity", class: "service", status: "succeeded", resumable: false, cancellable: false, createdAt: "2026-01-01T00:00:00Z" },
                { id: "j-2", type: "future_job", class: "future_class", status: "succeeded", resumable: false, cancellable: false, createdAt: "2026-01-01T00:01:00Z" },
              ],
            },
            response: { ok: true },
          })
        : Promise.resolve({ data: null, response: { ok: false } }),
    );

    render(
      <MemoryRouter>
        <JobsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByRole("link", { name: "Upgrade parity disk" })).toBeInTheDocument();
    expect(screen.getByText("Service")).toBeInTheDocument();
    expect(screen.queryByText("disk_upgrade_parity")).not.toBeInTheDocument();
    expect(screen.queryByText("service")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "future_job" })).toBeInTheDocument();
    expect(screen.getByText("future_class")).toBeInTheDocument();
  });

  it("labels the progress bar of a running job", async () => {
    mockGet.mockImplementation((path: string) =>
      path === "/jobs"
        ? Promise.resolve({
            data: {
              jobs: [
                { id: "j-1", type: "appdata_restore", class: "service", status: "running", progress: 40, resumable: false, cancellable: true, createdAt: "2026-01-01T00:00:00Z" },
              ],
            },
            response: { ok: true },
          })
        : Promise.resolve({ data: null, response: { ok: false } }),
    );

    render(
      <MemoryRouter>
        <JobsPage />
      </MemoryRouter>,
    );

    await screen.findByRole("link", { name: "Appdata restore" });
    expect(screen.getAllByText("Appdata restore")).toHaveLength(2);
    expect(screen.queryByText("appdata_restore")).not.toBeInTheDocument();
  });

  it("labels the type filter options", async () => {
    mockGet.mockResolvedValue({ data: { jobs: [] }, response: { ok: true } });

    render(
      <MemoryRouter>
        <JobsPage />
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("combobox", { name: "Type" }));
    expect(await screen.findByRole("option", { name: "Parity sync" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Mover" })).toBeInTheDocument();
  });
});

describe("job detail labels (#435)", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
  });

  it("uses the type label as the heading and the class label in the metadata", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/jobs/{jobId}") {
        return Promise.resolve({
          data: {
            id: "job-1",
            type: "disk_upgrade_parity",
            class: "topology",
            status: "succeeded",
            resumable: false,
            cancellable: false,
            createdAt: "2026-01-01T00:00:00Z",
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: "", response: { ok: true } });
    });

    render(
      <MemoryRouter initialEntries={["/jobs/job-1"]}>
        <Routes>
          <Route path="/jobs/:jobId" element={<JobDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    expect(await screen.findByRole("heading", { level: 1, name: "Upgrade parity disk" })).toBeInTheDocument();
    expect(screen.getByText("Class: Disk layout")).toBeInTheDocument();
    expect(screen.getAllByText("Upgrade parity disk")).toHaveLength(2);
    expect(screen.queryByText("disk_upgrade_parity")).not.toBeInTheDocument();
  });
});

describe("parity history and dashboard banner labels (#437)", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      addEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
      matches: false,
      media: query,
      onchange: null,
      removeEventListener: vi.fn(),
    })) as unknown as typeof window.matchMedia;
  });

  function mockApi(jobs: unknown[]): void {
    mockGet.mockImplementation((path: string) => {
      if (path === "/status") {
        return Promise.resolve({
          data: { healthy: true, summary: "OK", arrayDegraded: false, parityBlocked: false, activeJobs: 0 },
          response: { ok: true },
        });
      }
      if (path === "/pool") {
        return Promise.resolve({ data: { mounted: true, disks: [] }, response: { ok: true } });
      }
      if (path === "/jobs") {
        return Promise.resolve({ data: { jobs }, response: { ok: true } });
      }
      if (path === "/doctor") {
        return Promise.resolve({ data: { overall: "pass", checks: [] }, response: { ok: true } });
      }
      if (path === "/parity") {
        return Promise.resolve({ data: { freshness: "green", guard: { wouldBlock: false } }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
  }

  it("labels each job type in the parity history", async () => {
    mockApi([
      { id: "p-1", type: "sync", class: "parity", status: "succeeded", resumable: false, cancellable: false, createdAt: "2026-01-01T00:00:00Z" },
      { id: "p-2", type: "scrub", class: "parity", status: "failed", resumable: false, cancellable: false, createdAt: "2026-01-01T00:01:00Z" },
      { id: "p-3", type: "fix", class: "parity", status: "succeeded", resumable: false, cancellable: false, createdAt: "2026-01-01T00:02:00Z" },
    ]);

    const { container } = render(
      <MemoryRouter>
        <AppShell>
          <ParityPage />
        </AppShell>
      </MemoryRouter>,
    );

    await waitFor(() => expect(container.querySelector('[data-job-id="p-1"]')).not.toBeNull());
    const row = (id: string) => container.querySelector(`[data-job-id="${id}"]`) as HTMLElement;
    expect(within(row("p-1")).getByText("Parity sync")).toBeInTheDocument();
    expect(within(row("p-2")).getByText("Parity scrub")).toBeInTheDocument();
    expect(within(row("p-3")).getByText("Parity fix")).toBeInTheDocument();
    expect(within(row("p-1")).queryByText("sync")).not.toBeInTheDocument();
    expect(within(row("p-2")).queryByText("scrub")).not.toBeInTheDocument();
    expect(within(row("p-3")).queryByText("fix")).not.toBeInTheDocument();
  });

  it("names the failed job by its label in the dashboard banner", async () => {
    mockApi([
      { id: "f-1", type: "disk_upgrade_parity", class: "topology", status: "failed", resumable: false, cancellable: false, createdAt: "2026-01-01T00:00:00Z", error: { message: "disk vanished" } },
    ]);

    render(
      <MemoryRouter>
        <AppShell>
          <DashboardPage />
        </AppShell>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Job failed: Upgrade parity disk")).toBeInTheDocument();
    expect(screen.queryByText(/disk_upgrade_parity/)).not.toBeInTheDocument();
  });
});
