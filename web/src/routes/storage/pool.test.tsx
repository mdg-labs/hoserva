import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

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

function renderPool() {
  return render(
    <MemoryRouter>
      <AppShell>
        <PoolOverviewPage />
      </AppShell>
    </MemoryRouter>,
  );
}

function mockStatusAndJobs(get: typeof mockGet, poolData: unknown): void {
  get.mockImplementation((path: string) => {
    if (path === "/status") {
      return Promise.resolve({ data: { healthy: true, summary: "OK", maintenanceMode: false }, response: { ok: true } });
    }
    if (path === "/pool") {
      return Promise.resolve({ data: poolData, response: { ok: true } });
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

describe("PoolOverviewPage load failures", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockMatchMedia();
  });

  it("shows a load error instead of the unmounted capacity tile when /pool fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/status") {
        return Promise.resolve({ data: { healthy: true, summary: "OK" }, response: { ok: true } });
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

    renderPool();

    expect(await screen.findByText("pool unavailable")).toBeInTheDocument();
    expect(screen.queryByText("Unmounted")).not.toBeInTheDocument();
  });
});

describe("PoolOverviewPage missing disk", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockMatchMedia();
    mockStatusAndJobs(mockGet, poolWithMissingDisk());
  });

  it("renders a missing disk in its slot with a plain-language status badge", async () => {
    renderPool();

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

    // Unknown usage is shown as unknown, never as an empty disk.
    expect(within(missingTile as HTMLElement).getByText(/Usage unavailable/)).toBeInTheDocument();
    expect(within(missingTile as HTMLElement).queryByText(/0 B/)).not.toBeInTheDocument();

    // Every state is a catalog label, not the API enum.
    expect(screen.queryByText("active")).not.toBeInTheDocument();
    expect(screen.getByText("Active")).toBeInTheDocument();
  });
});

// mountedPool's own unassigned disk (/dev/sdf) is the candidate #288's own
// Add disk / Replace disk dialogs offer — the pool page filters
// PoolDiskEntry by role to build those device lists.
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
      {
        device: "/dev/sdf",
        mountPoint: "",
        role: "unassigned",
        state: "active",
        sizeBytes: 4_000_000_000_000,
        usedBytes: 0,
      },
    ],
  };
}

// poolWithMissingReplaceCandidate is mountedPool with its data slot
// replaced by a stored member no longer physically present (#326's own
// "missing" shape, state alone marking it, never an empty device string)
// — exactly the failed-disk slot doc 02 §4 "Replacing a failed disk"
// exists for, and finding 2 of #288's fix round requires the replace
// dialog be able to select. It is its own scenario (not mountedPool plus
// this slot) so the missing slot is the select's only, first-rendered
// option — this test suite's own comboboxes only support asserting a
// click on the first rendered option reliably in this environment.
function poolWithMissingReplaceCandidate() {
  return {
    mounted: true,
    disks: [
      {
        device: "/dev/sdx",
        mountPoint: "/mnt/disk4",
        role: "data",
        state: "missing",
      },
      {
        device: "/dev/sde",
        mountPoint: "/mnt/parity",
        role: "parity",
        state: "active",
        sizeBytes: 4_000_000_000_000,
        usedBytes: 400_000_000_000,
      },
      {
        device: "/dev/sdf",
        mountPoint: "",
        role: "unassigned",
        state: "active",
        sizeBytes: 4_000_000_000_000,
        usedBytes: 0,
      },
    ],
  };
}

describe("Pool overview page — add/replace disk (#288)", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockMatchMedia();
    mockStatusAndJobs(mockGet, mountedPool());
  });

  it("shows an add-disk plan error inside the dialog rather than closing it or formatting anything", async () => {
    mockPost.mockImplementation((path: string) => {
      if (path === "/disks/array/add/plan") {
        return Promise.resolve({
          error: { message: "disk: a parity disk must be at least as large as the largest data disk" },
          response: { ok: false },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Add disk" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Device" }));
    fireEvent.click(await screen.findByRole("option", { name: "/dev/sdf" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Preview" }));

    expect(
      await within(dialog).findByText("disk: a parity disk must be at least as large as the largest data disk"),
    ).toBeInTheDocument();
    // The plan call failed, so no typed-confirm field or submit button
    // ever appear — never a false "success" state (ui-states known escape).
    expect(within(dialog).queryByRole("textbox")).not.toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Add disk" })).not.toBeInTheDocument();
    expect(mockPost).not.toHaveBeenCalledWith("/disks/array/add", expect.anything());
  });

  it("disables the add-disk submit button until the typed confirmation exactly matches the plan, and submits the plan's own fields", async () => {
    mockPost.mockImplementation((path: string) => {
      if (path === "/disks/array/add/plan") {
        return Promise.resolve({
          data: {
            device: "/dev/sdf",
            filesystem: "xfs",
            adopt: false,
            mountpoint: "/mnt/disk2",
            confirmation: "ERASE /dev/sdf",
          },
          response: { ok: true },
        });
      }
      if (path === "/disks/array/add") {
        return Promise.resolve({ data: { id: "job-1", type: "disk_add", class: "topology", status: "queued" }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Add disk" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Device" }));
    fireEvent.click(await screen.findByRole("option", { name: "/dev/sdf" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Preview" }));

    const submit = await within(dialog).findByRole("button", { name: "Add disk" });
    expect(submit).toBeDisabled();

    const confirmInput = within(dialog).getByRole("textbox");
    fireEvent.change(confirmInput, { target: { value: "wrong" } });
    expect(submit).toBeDisabled();

    fireEvent.change(confirmInput, { target: { value: "ERASE /dev/sdf" } });
    expect(submit).not.toBeDisabled();

    fireEvent.click(submit);
    await waitFor(() =>
      expect(mockPost).toHaveBeenCalledWith(
        "/disks/array/add",
        expect.objectContaining({
          body: { device: "/dev/sdf", filesystem: "xfs", adopt: false, confirmation: "ERASE /dev/sdf" },
        }),
      ),
    );
  });

  it("shows a replace-disk plan's identity, rebuild command and requires its own typed confirmation before enabling submit", async () => {
    mockPost.mockImplementation((path: string) => {
      if (path === "/disks/array/replace/plan") {
        return Promise.resolve({
          data: {
            mountpoint: "/mnt/disk1",
            previousDevice: "/dev/sdb",
            replacementDevice: "/dev/sdf",
            model: "WDC WD40EFRX",
            wwn: "0xabc123",
            sizeBytes: 4_000_000_000_000,
            filesystem: "xfs",
            adopt: false,
            rebuild: "snapraid fix -d d1",
            confirmation: "ERASE /dev/sdf",
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Replace disk" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Disk to replace" }));
    fireEvent.click(await screen.findByRole("option", { name: "/mnt/disk1 (/dev/sdb)" }));
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Replacement device" }));
    fireEvent.click(await screen.findByRole("option", { name: "/dev/sdf" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Preview" }));

    expect(await within(dialog).findByText("Runs snapraid fix -d d1")).toBeInTheDocument();
    // The replacement's own identity (finding 3): model, WWN and size, not
    // just its /dev/sdX path.
    expect(within(dialog).getByText(/WDC WD40EFRX/)).toBeInTheDocument();
    expect(within(dialog).getByText(/WWN 0xabc123/)).toBeInTheDocument();

    const submit = within(dialog).getByRole("button", { name: "Replace disk" });
    expect(submit).toBeDisabled();

    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "ERASE /dev/sdf" } });
    expect(submit).not.toBeDisabled();
    expect(mockPost).not.toHaveBeenCalledWith("/disks/array/replace", expect.anything());
  });

  it("offers a missing data slot in the replace dialog's own slot list, and previews against its mountpoint (finding 2)", async () => {
    mockStatusAndJobs(mockGet, poolWithMissingReplaceCandidate());
    mockPost.mockImplementation((path: string) => {
      if (path === "/disks/array/replace/plan") {
        return Promise.resolve({
          data: {
            mountpoint: "/mnt/disk4",
            previousDevice: "/dev/sdx",
            replacementDevice: "/dev/sdf",
            filesystem: "xfs",
            adopt: false,
            rebuild: "snapraid fix -d d2",
            confirmation: "ERASE /dev/sdf",
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Replace disk" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Disk to replace" }));

    // /mnt/disk4 has a stored device (/dev/sdx) no longer present in
    // inventory (GetPool's own #326 "missing" shape) — it must still be
    // selectable, labelled plainly rather than by a device the operator
    // cannot see anywhere in the pool page's own inventory.
    const missingOption = await screen.findByRole("option", { name: "/mnt/disk4 (missing)" });
    fireEvent.click(missingOption);

    fireEvent.click(within(dialog).getByRole("combobox", { name: "Replacement device" }));
    fireEvent.click(await screen.findByRole("option", { name: "/dev/sdf" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Preview" }));

    // The preview call carries the missing slot's own mountpoint — proof
    // the selection actually reached the request, not just the dropdown.
    await waitFor(() =>
      expect(mockPost).toHaveBeenCalledWith(
        "/disks/array/replace/plan",
        expect.objectContaining({
          body: expect.objectContaining({ mountpoint: "/mnt/disk4", device: "/dev/sdf" }),
        }),
      ),
    );
    expect(await within(dialog).findByText("Confirm replacing /mnt/disk4")).toBeInTheDocument();
  });

  it("drops a stale add-disk plan response and never lets its confirmation authorize the fresh selection's submit", async () => {
    let resolveFirst: (value: unknown) => void = () => {};
    const firstPending = new Promise((resolve) => {
      resolveFirst = resolve;
    });
    let callCount = 0;
    mockPost.mockImplementation((path: string) => {
      if (path === "/disks/array/add/plan") {
        callCount += 1;
        if (callCount === 1) {
          // The first request (adopt=false) resolves only after the
          // second one (adopt=true) has already returned — the exact
          // race finding 4 describes. Its own confirmation phrase is
          // deliberately distinguishable from the second's own.
          return firstPending.then(() => ({
            data: {
              device: "/dev/sdf",
              filesystem: "xfs",
              adopt: false,
              mountpoint: "/mnt/disk2",
              confirmation: "STALE-PLAN-DO-NOT-USE",
            },
            response: { ok: true },
          }));
        }
        return Promise.resolve({
          data: {
            device: "/dev/sdf",
            filesystem: "xfs",
            adopt: true,
            mountpoint: "/mnt/disk2",
            confirmation: "ADOPT ONLY — NOTHING ERASED",
          },
          response: { ok: true },
        });
      }
      if (path === "/disks/array/add") {
        return Promise.resolve({ data: { id: "job-1", type: "disk_add", class: "topology", status: "queued" }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Add disk" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Device" }));
    fireEvent.click(await screen.findByRole("option", { name: "/dev/sdf" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Preview" }));

    // Change the adopt flag before the first request resolves — a
    // selection change mid-flight, the same shape switching the device
    // or filesystem would take.
    fireEvent.click(within(dialog).getByRole("switch", { name: "Adopt existing filesystem (keep its data)" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Preview" }));

    await waitFor(() => expect(callCount).toBe(2));
    resolveFirst(undefined);

    // The stale first response must never install its own plan or
    // confirmation phrase.
    await within(dialog).findByText(/Type exactly: ADOPT ONLY/);
    expect(within(dialog).queryByText(/STALE-PLAN-DO-NOT-USE/)).not.toBeInTheDocument();

    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "ADOPT ONLY — NOTHING ERASED" } });
    const submit = within(dialog).getByRole("button", { name: "Add disk" });
    expect(submit).not.toBeDisabled();
    fireEvent.click(submit);

    // Submits the fresh plan's own fields (adopt: true), never the stale
    // one's (adopt: false) and never raw, possibly-changed-again form
    // state.
    await waitFor(() =>
      expect(mockPost).toHaveBeenCalledWith(
        "/disks/array/add",
        expect.objectContaining({
          body: { device: "/dev/sdf", filesystem: "xfs", adopt: true, confirmation: "ADOPT ONLY — NOTHING ERASED" },
        }),
      ),
    );
  });
});
