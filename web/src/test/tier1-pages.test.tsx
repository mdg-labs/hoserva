import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi, beforeEach } from "vitest";

import { AppShell } from "@/components/patterns/app-shell";
import { GroupedResults } from "@/components/patterns/grouped-results";
import { sortParityDiffGroups } from "@/components/patterns/parity-diff";
import { JOB_STATUS_FILTER_VALUES } from "@/hooks/job-filter-options";
import { DashboardPage } from "@/routes/dashboard";
import { PoolOverviewPage } from "@/routes/storage/pool";
import { ParityPage } from "@/routes/storage/parity";
import { WakeEventsPage } from "@/routes/storage/wake-events";
import { JobsPage } from "@/routes/jobs/index";

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

function degradedStatus() {
  return {
    healthy: false,
    summary: "Degraded",
    arrayDegraded: true,
    parityBlocked: false,
    activeJobs: 1,
  };
}

function syncBlockedStatus() {
  return {
    healthy: true,
    summary: "Warnings",
    arrayDegraded: false,
    parityBlocked: true,
    activeJobs: 0,
  };
}

function rebuildingStatus() {
  return {
    healthy: true,
    summary: "Rebuilding",
    arrayDegraded: false,
    parityBlocked: false,
    activeJobs: 1,
  };
}

function mountedPool() {
  return {
    mounted: true,
    disks: [
      {
        device: "/dev/sdb",
        mountPoint: "/mnt/disk1",
        role: "data",
        state: "active",
        sizeBytes: 4_000_000_000_000,
        usedBytes: 2_000_000_000_000,
      },
      {
        device: "/dev/sde",
        mountPoint: "/mnt/parity",
        role: "parity",
        state: "active",
        sizeBytes: 4_000_000_000_000,
        usedBytes: 400_000_000_000,
      },
    ],
  };
}

function mockApiForStatus(status: Record<string, unknown>, jobs: unknown[] = []): void {
  mockGet.mockImplementation((path: string) => {
    if (path === "/status") {
      return Promise.resolve({ data: status, response: { ok: true } });
    }
    if (path === "/pool") {
      return Promise.resolve({ data: mountedPool(), response: { ok: true } });
    }
    if (path === "/jobs") {
      return Promise.resolve({ data: { jobs }, response: { ok: true } });
    }
    if (path === "/doctor") {
      return Promise.resolve({
        data: {
          overall: "warn",
          checks: [
            {
              id: "parity_freshness",
              name: "Parity freshness",
              status: "warn",
              message: "Synced 6h ago",
            },
          ],
        },
        response: { ok: true },
      });
    }
    if (path === "/parity") {
      return Promise.resolve({
        data: {
          freshness: "green",
          guard: { wouldBlock: false },
        },
        response: { ok: true },
      });
    }
    return Promise.resolve({ data: null, response: { ok: false } });
  });
}

function mockMatchMedia(prefersDark = false): void {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    addEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
    matches: prefersDark,
    media: query,
    onchange: null,
    removeEventListener: vi.fn(),
  })) as unknown as typeof window.matchMedia;
}

describe("Tier 1 pages", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockMatchMedia(false);
  });

  it("shows top bar and degraded banner from status", async () => {
    mockApiForStatus(degradedStatus(), [
      {
        id: "a1a1a1a1-0002-4000-8000-000000000001",
        type: "check",
        class: "parity",
        status: "failed",
        resumable: false,
        cancellable: false,
        createdAt: "2026-01-02T03:00:00Z",
      },
    ]);

    render(
      <MemoryRouter>
        <AppShell>
          <div>Page body</div>
        </AppShell>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Degraded")).toBeInTheDocument();
    expect(screen.getByText("Array degraded")).toBeInTheDocument();
    expect(screen.getByLabelText("Active jobs")).toBeInTheDocument();
    expect(screen.getByLabelText("Notifications")).toBeInTheDocument();
  });

  it("shows sync-blocked banner and parity chip", async () => {
    mockApiForStatus(syncBlockedStatus());

    render(
      <MemoryRouter>
        <AppShell>
          <div>Page body</div>
        </AppShell>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Sync blocked by threshold guard")).toBeInTheDocument();
    expect(screen.getByText("Sync blocked")).toBeInTheDocument();
  });

  it("shows rebuilding scenario active jobs in the top bar", async () => {
    mockApiForStatus(rebuildingStatus(), [
      {
        id: "a1a1a1a1-0003-4000-8000-000000000001",
        type: "fix",
        class: "parity",
        status: "running",
        progress: 63,
        resumable: false,
        cancellable: true,
        createdAt: "2026-01-03T09:00:00Z",
      },
    ]);

    render(
      <MemoryRouter>
        <AppShell>
          <div>Page body</div>
        </AppShell>
      </MemoryRouter>,
    );

    expect(await screen.findByText("1 active")).toBeInTheDocument();
  });

  it("lists removals before other parity diff groups", () => {
    const groups = sortParityDiffGroups([
      { category: "added", paths: ["/a"], count: 1 },
      { category: "removed", paths: ["/b"], count: 1 },
      { category: "updated", paths: ["/c"], count: 1 },
    ]);

    render(
      <GroupedResults
        groups={groups.map((group) => ({
          id: group.category,
          label: group.category,
          count: group.paths.length,
          items: group.paths,
          defaultOpen: group.category === "removed",
        }))}
      />,
    );

    const labels = screen.getAllByText(/added|removed|updated/);
    expect(labels[0]).toHaveTextContent("removed");
  });

  it("shows stop-array confirm copy listing what will stop", async () => {
    mockApiForStatus({ healthy: true, summary: "OK", maintenanceMode: false });

    render(
      <MemoryRouter>
        <AppShell>
          <PoolOverviewPage />
        </AppShell>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Stop array" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Shut down running VMs")).toBeInTheDocument();
    expect(within(dialog).getByText("Stop containers")).toBeInTheDocument();
    expect(within(dialog).getByText("Stop Samba and NFS")).toBeInTheDocument();
  });

  it("shows run-diff wake warning on the parity page", async () => {
    mockApiForStatus({ healthy: true, summary: "OK" });

    render(
      <MemoryRouter>
        <AppShell>
          <ParityPage />
        </AppShell>
      </MemoryRouter>,
    );

    expect(
      await screen.findByText("Run diff spins up every data disk to compare files against parity."),
    ).toBeInTheDocument();
  });

  it("shows cancel tooltip for non-cancellable jobs", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/jobs") {
        return Promise.resolve({
          data: {
            jobs: [
              {
                id: "job-1",
                type: "sync",
                class: "parity",
                status: "running",
                progress: 10,
                resumable: false,
                cancellable: false,
                createdAt: "2026-01-01T00:00:00Z",
              },
            ],
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <JobsPage />
      </MemoryRouter>,
    );

    await waitFor(() => {
      expect(screen.getByLabelText("This job cannot be cancelled")).toBeInTheDocument();
    });
  });

  it("includes interrupted in the job status filter values", () => {
    expect(JOB_STATUS_FILTER_VALUES).toContain("interrupted");
  });

  it("does not show a drift banner for a passing drift check", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/status") {
        return Promise.resolve({ data: { healthy: true, summary: "OK" }, response: { ok: true } });
      }
      if (path === "/pool") {
        return Promise.resolve({ data: mountedPool(), response: { ok: true } });
      }
      if (path === "/jobs") {
        return Promise.resolve({ data: { jobs: [] }, response: { ok: true } });
      }
      if (path === "/doctor") {
        return Promise.resolve({
          data: {
            overall: "pass",
            checks: [
              {
                id: "config_drift",
                name: "Config drift",
                status: "pass",
                message: "Generated files match SQLite",
              },
            ],
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <AppShell>
          <div>Page body</div>
        </AppShell>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Page body")).toBeInTheDocument();
    expect(screen.queryByText("Configuration drift detected")).not.toBeInTheDocument();
  });

  it("shows a load error on the dashboard instead of the no-array state", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/status") {
        return Promise.resolve({
          data: { healthy: true, summary: "OK" },
          response: { ok: true },
        });
      }
      if (path === "/pool") {
        return Promise.resolve({ error: { message: "pool unavailable" }, response: { ok: false } });
      }
      if (path === "/jobs") {
        return Promise.resolve({ data: { jobs: [] }, response: { ok: true } });
      }
      if (path === "/doctor") {
        return Promise.resolve({ data: { overall: "pass", checks: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <AppShell>
          <DashboardPage />
        </AppShell>
      </MemoryRouter>,
    );

    expect(await screen.findByText("pool unavailable")).toBeInTheDocument();
    expect(screen.queryByText("No array configured")).not.toBeInTheDocument();
  });

  it("does not invent green freshness when parity status is missing after a successful diff", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/status") {
        return Promise.resolve({
          data: { healthy: true, summary: "OK", parityBlocked: false },
          response: { ok: true },
        });
      }
      if (path === "/pool") {
        return Promise.resolve({ data: mountedPool(), response: { ok: true } });
      }
      if (path === "/jobs") {
        return Promise.resolve({ data: { jobs: [] }, response: { ok: true } });
      }
      if (path === "/doctor") {
        return Promise.resolve({
          data: {
            overall: "warn",
            checks: [
              {
                id: "parity_freshness",
                name: "Parity freshness",
                status: "warn",
                message: "Synced 6h ago",
              },
            ],
          },
          response: { ok: true },
        });
      }
      if (path === "/parity") {
        return Promise.resolve({ error: { message: "parity unavailable" }, response: { ok: false } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPost.mockResolvedValue({
      data: { groups: [], guard: { wouldBlock: false } },
      response: { ok: true },
    });

    render(
      <MemoryRouter>
        <AppShell>
          <ParityPage />
        </AppShell>
      </MemoryRouter>,
    );

    expect(await screen.findByText("parity unavailable")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Run diff" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Run diff anyway" }));

    await waitFor(() => {
      expect(mockPost).toHaveBeenCalled();
    });
    const freshnessBadges = screen.getAllByText("Synced 6h ago");
    expect(freshnessBadges.some((el) => el.className.includes("bg-success"))).toBe(false);
  });

  it("renders the latest completed wake duration on the wake-events page", async () => {
    const today = new Date().toISOString().slice(0, 10);
    mockGet.mockImplementation((path: string) => {
      if (path === "/disks/wake-events") {
        return Promise.resolve({
          data: {
            events: [
              {
                device: "/dev/sdb",
                fromState: "standby",
                toState: "active",
                at: "2026-09-19T10:00:00Z",
                awakeDurationSeconds: 3661,
              },
            ],
            dailyWakeCounts: [{ device: "/dev/sdb", date: today, count: 2 }],
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <WakeEventsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("Awake duration")).toBeInTheDocument();
    expect(screen.getByText("1h 1m 1s")).toBeInTheDocument();
    expect(screen.getByText("2")).toBeInTheDocument();
  });

  it("keeps the sync dialog open when the parity sync request fails", async () => {
    mockApiForStatus({ healthy: true, summary: "OK" });
    mockPost.mockResolvedValue({ error: { message: "sync refused" }, response: { ok: false } });

    render(
      <MemoryRouter>
        <AppShell>
          <ParityPage />
        </AppShell>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Sync now" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Sync now" }));

    expect(await screen.findByText("sync refused")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("does not warn about unsaved changes when the fix wizard is opened and closed", async () => {
    mockApiForStatus({ healthy: true, summary: "OK" });

    render(
      <MemoryRouter>
        <AppShell>
          <ParityPage />
        </AppShell>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Guided fix" }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await waitFor(() => {
      expect(screen.queryByText("Discard changes?")).not.toBeInTheDocument();
    });
  });
});
