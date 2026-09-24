import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { AppShell } from "@/components/patterns/app-shell";
import { CachePage } from "@/routes/storage/cache";

const mockGet = vi.fn();
const mockPost = vi.fn();
const mockPatch = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
    PATCH: (...args: unknown[]) => mockPatch(...args),
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

function mockMatchMedia(): void {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    addEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
    matches: false,
    media: query,
    onchange: null,
    removeEventListener: vi.fn(),
  })) as unknown as typeof window.matchMedia;
}

function renderCachePage() {
  return render(
    <MemoryRouter>
      <AppShell>
        <CachePage />
      </AppShell>
    </MemoryRouter>,
  );
}

function mockSystemData(overrides?: {
  shares?: unknown[];
  pool?: unknown;
  jobs?: unknown[];
}): void {
  mockGet.mockImplementation((path: string) => {
    if (path === "/status") {
      return Promise.resolve({
        data: { healthy: true, summary: "OK", maintenanceMode: false },
        response: { ok: true },
      });
    }
    if (path === "/pool") {
      return Promise.resolve({
        data:
          overrides?.pool ?? {
            mounted: true,
            disks: [
              {
                device: "/dev/sdc",
                mountPoint: "/mnt/cache",
                role: "cache",
                state: "active",
                sizeBytes: 1_000_000_000,
                usedBytes: 400_000_000,
              },
            ],
          },
        response: { ok: true },
      });
    }
    if (path === "/jobs") {
      return Promise.resolve({
        data: {
          jobs:
            overrides?.jobs ?? [
              {
                id: "job-mover-1",
                type: "mover",
                class: "array_write",
                status: "succeeded",
                resumable: true,
                cancellable: false,
                createdAt: "2026-01-02T04:00:00Z",
                startedAt: "2026-01-02T04:00:05Z",
                finishedAt: "2026-01-02T04:10:00Z",
              },
            ],
        },
        response: { ok: true },
      });
    }
    if (path === "/doctor") {
      return Promise.resolve({ data: { overall: "pass", checks: [] }, response: { ok: true } });
    }
    if (path === "/shares") {
      return Promise.resolve({
        data: {
          shares:
            overrides?.shares ?? [
              {
                name: "media",
                path: "/mnt/user/media",
                cacheMode: "cache-then-move",
                createPolicy: "mspmfs",
                smb: { enabled: true, guest: false, readOnly: false, browseable: true, recycle: false, timeMachine: false },
                nfs: { enabled: false, hosts: [], squash: "root_squash" },
                usage: null,
                createdAt: "2026-01-01T00:00:00Z",
                updatedAt: "2026-01-01T00:00:00Z",
              },
            ],
        },
        response: { ok: true },
      });
    }
    return Promise.resolve({ data: null, response: { ok: false } });
  });
}

describe("CachePage", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockPatch.mockReset();
    mockMatchMedia();
  });

  it("shows cache disk fill and the most recent mover job timing", async () => {
    mockSystemData();

    renderCachePage();

    expect(await screen.findByText("Cache & mover")).toBeInTheDocument();
    expect(screen.getByText("/dev/sdc")).toBeInTheDocument();
    expect(screen.getByText("Succeeded")).toBeInTheDocument();
    expect(screen.getByText(/^Started .+ · 9m 55s$/)).toBeInTheDocument();
  });

  it("shows the parity warning when a cache-only share exists", async () => {
    mockSystemData({
      shares: [
        {
          name: "appdata",
          path: "/mnt/user/appdata",
          cacheMode: "cache-only",
          createPolicy: "mspmfs",
          smb: { enabled: false, guest: false, readOnly: false, browseable: true, recycle: false, timeMachine: false },
          nfs: { enabled: false, hosts: [], squash: "root_squash" },
          usage: null,
          createdAt: "2026-01-01T00:00:00Z",
          updatedAt: "2026-01-01T00:00:00Z",
        },
      ],
    });

    renderCachePage();

    expect(await screen.findByText("Cache-only data is not covered by parity")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open backup settings" })).toHaveAttribute("href", "/settings/backup");
  });

  it("queues a mover job from Run mover", async () => {
    mockSystemData();
    mockPost.mockResolvedValue({
      data: {
        id: "job-mover-new",
        type: "mover",
        class: "array_write",
        status: "queued",
        resumable: true,
        cancellable: false,
        createdAt: "2026-01-03T00:00:00Z",
      },
      response: { ok: true },
    });

    renderCachePage();
    fireEvent.click(await screen.findByRole("button", { name: "Run mover" }));

    await vi.waitFor(() => {
      expect(mockPost).toHaveBeenCalledWith("/mover/run");
    });
  });

  it.each([
    ["Change mode only", mockPatch],
    ["Change mode and relocate", mockPatch],
    ["Change mode and relocate", mockPost],
  ] as const)("shows a rejected request in the mode dialog (%s, %#)", async (button, rejecting) => {
    mockSystemData();
    mockPatch.mockResolvedValue({ data: { name: "media", cacheMode: "array-only" }, response: { ok: true } });
    rejecting.mockRejectedValue(new Error("network down"));

    renderCachePage();
    fireEvent.click(await screen.findByRole("combobox", { name: /media/ }));
    // Base UI's SelectItem commits on the pointer sequence, not on a bare click.
    const option = await screen.findByRole("option", { name: "Array only" });
    fireEvent.pointerDown(option, { pointerType: "mouse" });
    fireEvent.pointerUp(option, { pointerType: "mouse" });
    fireEvent.click(option);
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: button }));

    expect(await within(dialog).findByText("network down")).toBeInTheDocument();
  });

  it("shows a load error instead of cache content when /shares fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/status") {
        return Promise.resolve({
          data: { healthy: true, summary: "OK", maintenanceMode: false },
          response: { ok: true },
        });
      }
      if (path === "/pool") {
        return Promise.resolve({ data: { mounted: true, disks: [] }, response: { ok: true } });
      }
      if (path === "/jobs") {
        return Promise.resolve({ data: { jobs: [] }, response: { ok: true } });
      }
      if (path === "/doctor") {
        return Promise.resolve({ data: { overall: "pass", checks: [] }, response: { ok: true } });
      }
      if (path === "/shares") {
        return Promise.resolve({
          error: { code: "internal_error", message: "shares unavailable" },
          response: { ok: false },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderCachePage();

    expect(await screen.findByText("shares unavailable")).toBeInTheDocument();
  });
});
