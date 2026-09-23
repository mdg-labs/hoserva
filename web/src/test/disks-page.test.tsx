import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi, beforeEach } from "vitest";

import { DisksPage } from "@/routes/storage/disks";

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

function inventory() {
  return {
    disks: [
      { device: "/dev/sdb", model: "WDC", serial: "DATA1", sizeBytes: 4_000_000_000_000, smartStatus: "ok" },
      { device: "/dev/sdf", model: "Samsung Portable SSD", serial: "USB1", sizeBytes: 1_000_000_000_000, smartStatus: "ok" },
    ],
  };
}

function pool() {
  return {
    mounted: true,
    disks: [
      {
        device: "/dev/sdb",
        mountPoint: "/mnt/disk1",
        role: "data",
        state: "active",
        sizeBytes: 4_000_000_000_000,
        usedBytes: 1_000_000_000_000,
      },
    ],
  };
}

function external() {
  return {
    disks: [
      {
        label: "backup",
        device: "/dev/sdf",
        mountPoint: "/mnt/disks/backup",
        containerPath: "/mnt/disks/backup",
        mounted: false,
        backupDestination: true,
        boot: false,
        filesystem: "xfs",
        fsUuid: "uuid-ext",
      },
    ],
  };
}

describe("DisksPage external group", () => {
  beforeEach(() => {
    cleanup();
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      addEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
      matches: false,
      media: query,
      onchange: null,
      removeEventListener: vi.fn(),
    })) as unknown as typeof window.matchMedia;
    mockGet.mockReset();
    mockPost.mockReset();
    mockPatch.mockReset();
    mockGet.mockImplementation((path: string) => {
      if (path === "/disks") {
        return Promise.resolve({ data: inventory(), response: { ok: true } });
      }
      if (path === "/pool") {
        return Promise.resolve({ data: pool(), response: { ok: true } });
      }
      if (path === "/disks/external") {
        return Promise.resolve({ data: external(), response: { ok: true } });
      }
      return Promise.resolve({ data: {}, response: { ok: true } });
    });
    mockPost.mockResolvedValue({ data: external().disks[0], response: { ok: true } });
    mockPatch.mockResolvedValue({ data: external().disks[0], response: { ok: true } });
  });

  it("lists external disks in their own group and keeps them out of the array table", async () => {
    render(
      <MemoryRouter>
        <DisksPage />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("External disks")).toBeInTheDocument());
    expect(screen.getByText("backup")).toBeInTheDocument();
    expect(screen.getAllByText("/mnt/disks/backup").length).toBeGreaterThanOrEqual(1);
    expect(screen.queryAllByText("/dev/sdf")).toHaveLength(1);
    expect(screen.getByText("/dev/sdb")).toBeInTheDocument();
  });

  it("mounts an external disk from row actions", async () => {
    render(
      <MemoryRouter>
        <DisksPage />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByLabelText("Actions for backup")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText("Actions for backup"));
    fireEvent.click(await screen.findByText("Mount"));
    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    expect(mockPost.mock.calls[0][0]).toBe("/disks/external/{label}/mount");
  });

  it("shows a load error instead of treating every disk as unassigned when pool fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/disks") {
        return Promise.resolve({ data: inventory(), response: { ok: true } });
      }
      if (path === "/pool") {
        return Promise.resolve({ error: { message: "pool unavailable" }, response: { ok: false } });
      }
      if (path === "/disks/external") {
        return Promise.resolve({ data: external(), response: { ok: true } });
      }
      return Promise.resolve({ data: {}, response: { ok: true } });
    });
    render(
      <MemoryRouter>
        <DisksPage />
      </MemoryRouter>,
    );
    expect(await screen.findByText("pool unavailable")).toBeInTheDocument();
    expect(screen.queryByText("unassigned")).not.toBeInTheDocument();
  });

  it("mounts a listed unregistered disk even when the list omitted fsUuid", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/disks") {
        return Promise.resolve({ data: inventory(), response: { ok: true } });
      }
      if (path === "/pool") {
        return Promise.resolve({ data: pool(), response: { ok: true } });
      }
      if (path === "/disks/external") {
        const disk = {
          label: "backup",
          device: "/dev/sdf",
          mountPoint: "/mnt/disks/backup",
          containerPath: "/mnt/disks/backup",
          mounted: false,
          backupDestination: false,
          boot: false,
          filesystem: "xfs",
        };
        return Promise.resolve({ data: { disks: [disk] }, response: { ok: true } });
      }
      return Promise.resolve({ data: {}, response: { ok: true } });
    });
    render(
      <MemoryRouter>
        <DisksPage />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByLabelText("Actions for backup")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText("Actions for backup"));
    fireEvent.click(await screen.findByText("Mount"));
    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    expect(mockPost.mock.calls[0][0]).toBe("/disks/external/{label}/mount");
  });
});
