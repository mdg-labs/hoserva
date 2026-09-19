import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { TypedConfirm } from "@/components/patterns/typed-confirm";
import { AuthProvider } from "@/lib/api/auth-guard";
import { StorageSetupPage } from "@/routes/storage-setup";
import {
  buildConfirmPhrase,
  buildCreateArrayRequest,
  disksToErase,
  validateRoleAssignment,
  type DiskEntry,
  type DiskRole,
} from "@/routes/storage-setup/validation";

const mockGet = vi.fn();
const mockPost = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  },
}));

const DISK_SDB: DiskEntry = {
  device: "/dev/sdb",
  sizeBytes: 4_000_000_000_000,
  model: "WDC WD40EFRX",
  serial: "WD-WCC4E1234567",
  boot: false,
};

const DISK_SDC: DiskEntry = {
  device: "/dev/sdc",
  sizeBytes: 4_000_000_000_000,
  model: "WDC WD40EFRX",
  serial: "WD-WCC4E7654321",
  boot: false,
};

const DISK_SMALL: DiskEntry = {
  device: "/dev/sdd",
  sizeBytes: 2_000_000_000_000,
  boot: false,
};

const DISK_WEAK: DiskEntry = {
  device: "/dev/sde",
  sizeBytes: 4_000_000_000_000,
  boot: false,
  weakIdentity: true,
};

function renderSetup(): ReturnType<typeof render> {
  return render(
    <MemoryRouter>
      <AuthProvider>
        <StorageSetupPage />
      </AuthProvider>
    </MemoryRouter>,
  );
}

function mockFreshInstall(disks: DiskEntry[] = [DISK_SDB, DISK_SDC]): void {
  mockGet.mockImplementation((path: string) => {
    if (path === "/setup/status") {
      return Promise.resolve({ data: { adminExists: true }, response: { ok: true } });
    }
    if (path === "/auth/session") {
      return Promise.resolve({
        data: { id: "00000000-0000-0000-0000-000000000001", username: "admin", role: "admin", totpEnrolled: false },
        response: { ok: true },
      });
    }
    if (path === "/pool") {
      return Promise.resolve({ data: { mounted: false, disks: [] }, response: { ok: true } });
    }
    if (path === "/disks") {
      return Promise.resolve({ data: { disks }, response: { ok: true } });
    }
    return Promise.resolve({ data: null, response: { ok: false } });
  });
}

async function goToRoleStep(): Promise<void> {
  renderSetup();
  await waitFor(() => expect(screen.getByText("/dev/sdb")).toBeInTheDocument());
  fireEvent.click(screen.getByRole("button", { name: "Next" }));
  await waitFor(() => expect(screen.getByText("Role assignment")).toBeInTheDocument());
}

describe("storage setup validation", () => {
  it("refuses more than two parity disks", () => {
    const disks = [DISK_SDB, DISK_SDC, { ...DISK_SMALL, device: "/dev/sdf" }, { ...DISK_SMALL, device: "/dev/sdg" }];
    const roles: Record<string, DiskRole> = {
      "/dev/sdb": "parity",
      "/dev/sdc": "parity",
      "/dev/sdf": "parity",
      "/dev/sdg": "data",
    };
    const result = validateRoleAssignment(disks, roles);
    expect(result.errorCodes).toContain("tooManyParity");
  });

  it("refuses parity smaller than the largest data disk", () => {
    const roles: Record<string, DiskRole> = {
      "/dev/sdd": "parity",
      "/dev/sdb": "data",
    };
    const result = validateRoleAssignment([DISK_SMALL, DISK_SDB], roles);
    expect(result.errorCodes).toContain("parityTooSmall");
    expect(result.errorsByDevice["/dev/sdd"]).toBe("parityTooSmall");
  });

  it("refuses weak-identity disks as parity", () => {
    const roles: Record<string, DiskRole> = {
      "/dev/sde": "parity",
      "/dev/sdb": "data",
    };
    const result = validateRoleAssignment([DISK_WEAK, DISK_SDB], roles);
    expect(result.errorCodes).toContain("weakIdentityParity");
    expect(result.errorsByDevice["/dev/sde"]).toBe("weakIdentityParity");
  });

  it("lists every disk to erase for typed confirmation, including cache", () => {
    const roles: Record<string, DiskRole> = {
      "/dev/sdb": "parity",
      "/dev/sdc": "data",
    };
    const erased = disksToErase([DISK_SDB, DISK_SDC], roles, { "/dev/sdc": "format" });
    expect(erased).toEqual(["/dev/sdb", "/dev/sdc"]);
    expect(buildConfirmPhrase(erased)).toBe("ERASE /dev/sdb, /dev/sdc");

    const withCache = disksToErase(
      [DISK_SDB, DISK_SDC, DISK_SMALL],
      { "/dev/sdb": "parity", "/dev/sdc": "data", "/dev/sdd": "cache" },
      { "/dev/sdc": "keep" },
    );
    expect(withCache).toEqual(["/dev/sdb", "/dev/sdd"]);
    expect(buildConfirmPhrase(withCache)).toBe("ERASE /dev/sdb, /dev/sdd");
    expect(buildConfirmPhrase([])).toBe("ADOPT ONLY — NOTHING ERASED");
  });

  it("builds a createArray body that matches TopologyPlan confirmation", () => {
    const body = buildCreateArrayRequest(
      [DISK_SDB, DISK_SDC],
      { "/dev/sdb": "parity", "/dev/sdc": "data" },
      { "/dev/sdc": "format" },
      "mspmfs",
      50,
      "ERASE /dev/sdb, /dev/sdc",
    );
    expect(body).toEqual({
      disks: [
        { device: "/dev/sdb", role: "parity", filesystem: "xfs", adopt: false },
        { device: "/dev/sdc", role: "data", filesystem: "xfs", adopt: false },
      ],
      createPolicy: "mspmfs",
      minFreeSpace: "50G",
      confirmation: "ERASE /dev/sdb, /dev/sdc",
    });
  });
});

describe("StorageSetupPage", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockPost.mockResolvedValue({
      data: {
        id: "00000000-0000-0000-0000-000000000099",
        type: "disk_format",
        class: "topology",
        status: "queued",
        resumable: false,
        cancellable: false,
        createdAt: "2026-09-19T00:00:00Z",
      },
      response: { ok: true },
    });
    localStorage.clear();
    sessionStorage.clear();
    mockFreshInstall();
  });

  it("shows live validation when parity is too small", async () => {
    mockFreshInstall([DISK_SMALL, DISK_SDB]);
    await goToRoleStep();

    expect(screen.getByText("Fix these problems before continuing")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Next" })).toBeDisabled();
  });

  it("lists every disk to erase in typed confirmation", () => {
    const phrase = buildConfirmPhrase(["/dev/sdb", "/dev/sdc"]);
    render(
      <TypedConfirm
        phrase={phrase}
        value=""
        onChange={() => undefined}
        title="Disks that will be erased"
        description="These disks will be formatted."
        items={["/dev/sdb — all data will be erased", "/dev/sdc — all data will be erased"]}
      />,
    );

    expect(screen.getByText("/dev/sdb — all data will be erased")).toBeInTheDocument();
    expect(screen.getByText("/dev/sdc — all data will be erased")).toBeInTheDocument();
    expect(screen.getByText("Type exactly: ERASE /dev/sdb, /dev/sdc")).toBeInTheDocument();
  });
});
