import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi, beforeEach } from "vitest";

import { AppShell } from "@/components/patterns/app-shell";

import { PoolOverviewPage } from "@/routes/storage/pool";

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

// A stored array member with no identity match anywhere in inventory
// (#326) — GetPool's own shape for it: stored device/role/mountpoint,
// state missing, no size/used.
function poolWithMissingDisk() {
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
        device: "/dev/sdc",
        mountPoint: "/mnt/disk2",
        role: "data",
        state: "missing",
      },
    ],
  };
}

function mockApi(): void {
  mockGet.mockImplementation((path: string) => {
    if (path === "/status") {
      return Promise.resolve({ data: { healthy: true, summary: "OK" }, response: { ok: true } });
    }
    if (path === "/pool") {
      return Promise.resolve({ data: poolWithMissingDisk(), response: { ok: true } });
    }
    if (path === "/jobs") {
      return Promise.resolve({ data: { jobs: [] }, response: { ok: true } });
    }
    if (path === "/doctor") {
      return Promise.resolve({ data: { overall: "pass", checks: [] }, response: { ok: true } });
    }
    return Promise.resolve({ data: null, response: { ok: false } });
  });
}

describe("PoolOverviewPage missing disk", () => {
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
    mockApi();
  });

  it("renders a missing disk in its slot with a plain-language status badge", async () => {
    render(
      <MemoryRouter>
        <AppShell>
          <PoolOverviewPage />
        </AppShell>
      </MemoryRouter>,
    );

    // The present disk keeps today's rendering.
    await waitFor(() => expect(screen.getByText("/dev/sdb")).toBeInTheDocument());

    // The missing disk still gets its own tile, at its stored device and
    // mountpoint, distinct from the present one — never dropped.
    const missingTile = screen.getByText("/dev/sdc").closest('[data-slot="card"]');
    expect(missingTile).not.toBeNull();
    expect(within(missingTile as HTMLElement).getByText("/mnt/disk2")).toBeInTheDocument();

    // Never the raw enum value — a plain-language label instead.
    expect(screen.queryByText("missing")).not.toBeInTheDocument();
    expect(screen.getByText("Missing — not detected")).toBeInTheDocument();
  });
});
