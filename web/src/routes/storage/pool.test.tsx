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

// poolWithRemovalState is mountedPool with its second data disk
// (/mnt/disk2) carrying removalState and its own server-supplied
// finishConfirmation phrase (PoolDiskEntry, #361, mirroring production's
// GetPool: set whenever removalState is set, not only when the disk is
// still eligible for a fresh planDiskEvacuation call) — #361's own
// per-state fixture for the removal badge, Finish removal and Cancel
// removal.
function poolWithRemovalState(state: string) {
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
        state: "active",
        sizeBytes: 4_000_000_000_000,
        usedBytes: 1_000_000_000_000,
        removalState: state,
        finishConfirmation: "REMOVE /mnt/disk2",
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

// mockPoolAndJobs is mockStatusAndJobs's own shape, with a caller-supplied
// jobs list (default []) and an optional /disks response — #361's Finish
// removal dialog reads inventory once, for the ending "safe to physically
// remove" message's own device identity.
function mockPoolAndJobs(
  get: typeof mockGet,
  poolData: unknown,
  jobs: unknown[] = [],
  disksEntries: unknown[] = [],
): void {
  get.mockImplementation((path: string) => {
    if (path === "/status") {
      return Promise.resolve({ data: { healthy: true, summary: "OK", maintenanceMode: false }, response: { ok: true } });
    }
    if (path === "/pool") {
      return Promise.resolve({ data: poolData, response: { ok: true } });
    }
    if (path === "/jobs") {
      return Promise.resolve({ data: { jobs }, response: { ok: true } });
    }
    if (path === "/doctor") {
      return Promise.resolve({ data: { overall: "pass", checks: [] }, response: { ok: true } });
    }
    if (path === "/disks") {
      return Promise.resolve({ data: { disks: disksEntries }, response: { ok: true } });
    }
    return Promise.resolve({ data: null, response: { ok: false } });
  });
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

describe("Pool overview page — rebalance and remove disk (#274)", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockMatchMedia();
    mockStatusAndJobs(mockGet, mountedPool());
  });

  it("previews the rebalance plan as soon as the dialog opens, and shows a failed preview's error inside it", async () => {
    mockPost.mockImplementation((path: string) => {
      if (path === "/pool/rebalance/plan") {
        return Promise.resolve({ error: { message: "rebalance plan unavailable" }, response: { ok: false } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Rebalance" }));
    const dialog = await screen.findByRole("dialog");

    expect(await within(dialog).findByText("rebalance plan unavailable")).toBeInTheDocument();
    // The plan call failed, so no typed-confirm field or submit button
    // ever appear — never a false "success" state (ui-states known escape).
    expect(within(dialog).queryByRole("textbox")).not.toBeInTheDocument();
    expect(mockPost).not.toHaveBeenCalledWith("/pool/rebalance", expect.anything());
  });

  it("requires the rebalance dialog's own typed confirmation before enabling submit, and posts confirmation alone", async () => {
    mockPost.mockImplementation((path: string) => {
      if (path === "/pool/rebalance/plan") {
        return Promise.resolve({
          data: {
            moves: [{ share: "media", relPath: "movie.mkv", sourceBranch: "/mnt/disk1/media", targetBranch: "/mnt/disk2/media", sizeBytes: 1_000_000 }],
            warnings: [],
            confirmation: "REBALANCE",
          },
          response: { ok: true },
        });
      }
      if (path === "/pool/rebalance") {
        return Promise.resolve({ data: { id: "job-1", type: "rebalance", class: "array_write", status: "queued" }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Rebalance" }));
    const dialog = await screen.findByRole("dialog");

    const submit = await within(dialog).findByRole("button", { name: "Rebalance" });
    expect(submit).toBeDisabled();
    expect(within(dialog).getByText(/^1 file, .+ total$/)).toBeInTheDocument();

    const confirmInput = within(dialog).getByRole("textbox");
    fireEvent.change(confirmInput, { target: { value: "wrong" } });
    expect(submit).toBeDisabled();

    fireEvent.change(confirmInput, { target: { value: "REBALANCE" } });
    expect(submit).not.toBeDisabled();

    fireEvent.click(submit);
    await waitFor(() =>
      expect(mockPost).toHaveBeenCalledWith("/pool/rebalance", expect.objectContaining({ body: { confirmation: "REBALANCE" } })),
    );
  });

  it("shows an already-balanced pool with no moves and never offers a submit button for it", async () => {
    mockPost.mockImplementation((path: string) => {
      if (path === "/pool/rebalance/plan") {
        return Promise.resolve({ data: { moves: [], warnings: [], confirmation: "REBALANCE" }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Rebalance" }));
    const dialog = await screen.findByRole("dialog");

    expect(await within(dialog).findByText(/already balanced/)).toBeInTheDocument();
    expect(within(dialog).queryByRole("textbox")).not.toBeInTheDocument();
  });

  it("shows a remove-disk plan error inside the dialog rather than closing it or starting a job", async () => {
    mockPost.mockImplementation((path: string) => {
      if (path === "/disks/array/evacuate/plan") {
        return Promise.resolve({ error: { message: "remaining disks do not have room to evacuate this disk" }, response: { ok: false } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Remove disk" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Disk to remove" }));
    fireEvent.click(await screen.findByRole("option", { name: "/mnt/disk1 (/dev/sdb)" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Preview" }));

    expect(await within(dialog).findByText("remaining disks do not have room to evacuate this disk")).toBeInTheDocument();
    expect(within(dialog).queryByRole("textbox")).not.toBeInTheDocument();
    expect(mockPost).not.toHaveBeenCalledWith("/disks/array/evacuate", expect.anything());
  });

  it("requires the remove-disk dialog's own typed confirmation before enabling submit, and posts the plan's own mountpoint", async () => {
    mockPost.mockImplementation((path: string) => {
      if (path === "/disks/array/evacuate/plan") {
        return Promise.resolve({
          data: {
            mountpoint: "/mnt/disk1",
            moves: [{ share: "media", relPath: "movie.mkv", sourceBranch: "/mnt/disk1/media", targetBranch: "/mnt/disk2/media", sizeBytes: 1_000_000 }],
            warnings: [],
            confirmation: "REMOVE /mnt/disk1",
          },
          response: { ok: true },
        });
      }
      if (path === "/disks/array/evacuate") {
        return Promise.resolve({ data: { id: "job-1", type: "evacuation", class: "array_write", status: "queued" }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Remove disk" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Disk to remove" }));
    fireEvent.click(await screen.findByRole("option", { name: "/mnt/disk1 (/dev/sdb)" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Preview" }));

    expect(await within(dialog).findByText("Confirm evacuating /mnt/disk1")).toBeInTheDocument();
    const submit = within(dialog).getByRole("button", { name: "Evacuate disk" });
    expect(submit).toBeDisabled();

    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "wrong" } });
    expect(submit).toBeDisabled();

    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "REMOVE /mnt/disk1" } });
    expect(submit).not.toBeDisabled();

    fireEvent.click(submit);
    await waitFor(() =>
      expect(mockPost).toHaveBeenCalledWith(
        "/disks/array/evacuate",
        expect.objectContaining({ body: { mountpoint: "/mnt/disk1", confirmation: "REMOVE /mnt/disk1" } }),
      ),
    );
  });
});

describe("Pool overview page — disk removal state, Finish removal, Cancel removal (#361)", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockMatchMedia();
  });

  it("shows the evacuating badge, and offers neither Finish removal nor Cancel removal", async () => {
    mockPoolAndJobs(mockGet, poolWithRemovalState("evacuating"));

    renderPool();

    expect(await screen.findByText("Being emptied — no new files land here")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Finish removal" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Cancel removal" })).not.toBeInTheDocument();
  });

  it("shows the evacuated badge, and offers both Finish removal and Cancel removal", async () => {
    mockPoolAndJobs(mockGet, poolWithRemovalState("evacuated"));

    renderPool();

    expect(await screen.findByText("Empty — ready to finish removal")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Finish removal" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel removal" })).toBeInTheDocument();
  });

  it.each(["unpooled", "unlisted"])("shows the %s badge, and offers Finish removal but not Cancel removal", async (state) => {
    mockPoolAndJobs(mockGet, poolWithRemovalState(state));

    renderPool();

    expect(await screen.findByText("Removal in progress")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Finish removal" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Cancel removal" })).not.toBeInTheDocument();
  });

  it("Finish removal can be retried for an unpooled disk — planDiskEvacuation would refuse it, but the dialog opens with a typeable confirmation anyway", async () => {
    // A disk_remove job that failed (e.g. the threshold guard tripped)
    // leaves the disk unpooled: still off the pool, but not yet unlisted
    // (finding 1 of this issue's fix round). planDiskEvacuation refuses
    // any disk that has left the pool with disk_leaving_array, so a
    // dialog that sourced its confirmation phrase from that operation
    // could never reopen here — this proves the dialog no longer calls it
    // at all.
    mockPoolAndJobs(mockGet, poolWithRemovalState("unpooled"));
    mockPost.mockImplementation((path: string) => {
      if (path === "/disks/array/evacuate/plan") {
        return Promise.resolve({
          error: { message: "disk /mnt/disk2 is being removed from the array (unpooled)" },
          response: { ok: false },
        });
      }
      if (path === "/disks/array/remove/finish") {
        return Promise.resolve({
          data: { id: "job-finish-2", type: "disk_remove", class: "topology", status: "queued", resumable: false, cancellable: false, createdAt: "2026-01-01T00:00:00Z" },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Finish removal" }));
    const dialog = await screen.findByRole("dialog");

    const confirmInput = await within(dialog).findByRole("textbox");
    const submit = within(dialog).getByRole("button", { name: "Finish removal" });
    expect(submit).toBeDisabled();

    fireEvent.change(confirmInput, { target: { value: "REMOVE /mnt/disk2" } });
    expect(submit).not.toBeDisabled();

    fireEvent.click(submit);
    await waitFor(() =>
      expect(mockPost).toHaveBeenCalledWith(
        "/disks/array/remove/finish",
        expect.objectContaining({ body: { mountpoint: "/mnt/disk2", confirmation: "REMOVE /mnt/disk2" } }),
      ),
    );
    expect(mockPost).not.toHaveBeenCalledWith("/disks/array/evacuate/plan", expect.anything());
  });

  it("renders nothing extra when no disk is in removal", async () => {
    mockPoolAndJobs(mockGet, mountedPool());

    renderPool();

    await waitFor(() => expect(screen.getByText("/mnt/disk1")).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: "Finish removal" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Cancel removal" })).not.toBeInTheDocument();
    expect(screen.queryByText(/Removal in progress/)).not.toBeInTheDocument();
  });

  it("Finish removal requires the exact typed phrase, calls finishDiskRemoval, shows the job's progress, and ends with the safe-to-remove message naming the device and serial/WWN", async () => {
    let financeCalled = false;
    mockPoolAndJobs(mockGet, poolWithRemovalState("evacuated"), [], [
      { device: "/dev/sdc", sizeBytes: 4_000_000_000_000, model: "WDC WD40EFRX", wwn: "0xabc123", boot: false },
    ]);
    mockGet.mockImplementation((path: string) => {
      if (path === "/status") {
        return Promise.resolve({ data: { healthy: true, summary: "OK", maintenanceMode: false }, response: { ok: true } });
      }
      if (path === "/pool") {
        return Promise.resolve({ data: poolWithRemovalState("evacuated"), response: { ok: true } });
      }
      if (path === "/jobs") {
        return Promise.resolve({
          data: {
            jobs: financeCalled
              ? [
                  {
                    id: "job-finish-1",
                    type: "disk_remove",
                    class: "topology",
                    status: "succeeded",
                    resumable: false,
                    cancellable: false,
                    createdAt: "2026-01-01T00:00:00Z",
                  },
                ]
              : [],
          },
          response: { ok: true },
        });
      }
      if (path === "/doctor") {
        return Promise.resolve({ data: { overall: "pass", checks: [] }, response: { ok: true } });
      }
      if (path === "/disks") {
        return Promise.resolve({
          data: { disks: [{ device: "/dev/sdc", sizeBytes: 4_000_000_000_000, model: "WDC WD40EFRX", wwn: "0xabc123", boot: false }] },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPost.mockImplementation((path: string) => {
      if (path === "/disks/array/evacuate/plan") {
        return Promise.resolve({
          data: { mountpoint: "/mnt/disk2", moves: [], warnings: [], confirmation: "REMOVE /mnt/disk2" },
          response: { ok: true },
        });
      }
      if (path === "/disks/array/remove/finish") {
        financeCalled = true;
        return Promise.resolve({
          data: {
            id: "job-finish-1",
            type: "disk_remove",
            class: "topology",
            status: "queued",
            resumable: false,
            cancellable: false,
            createdAt: "2026-01-01T00:00:00Z",
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Finish removal" }));
    const dialog = await screen.findByRole("dialog");

    const submit = await within(dialog).findByRole("button", { name: "Finish removal" });
    expect(submit).toBeDisabled();

    const confirmInput = await within(dialog).findByRole("textbox");
    fireEvent.change(confirmInput, { target: { value: "wrong" } });
    expect(submit).toBeDisabled();

    fireEvent.change(confirmInput, { target: { value: "REMOVE /mnt/disk2" } });
    expect(submit).not.toBeDisabled();

    fireEvent.click(submit);
    await waitFor(() =>
      expect(mockPost).toHaveBeenCalledWith(
        "/disks/array/remove/finish",
        expect.objectContaining({ body: { mountpoint: "/mnt/disk2", confirmation: "REMOVE /mnt/disk2" } }),
      ),
    );

    expect(
      await within(dialog).findByText(
        "/mnt/disk2 (WDC WD40EFRX · WWN 0xabc123 · 3.64 TiB) is safe to physically remove. Its filesystem was not wiped.",
      ),
    ).toBeInTheDocument();
  });

  it("shows a rejected Finish removal request inside the dialog without closing it", async () => {
    mockPoolAndJobs(mockGet, poolWithRemovalState("evacuated"));
    mockPost.mockImplementation((path: string) => {
      if (path === "/disks/array/evacuate/plan") {
        return Promise.resolve({
          data: { mountpoint: "/mnt/disk2", moves: [], warnings: [], confirmation: "REMOVE /mnt/disk2" },
          response: { ok: true },
        });
      }
      if (path === "/disks/array/remove/finish") {
        return Promise.resolve({ error: { message: "threshold guard tripped" }, response: { ok: false } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Finish removal" }));
    const dialog = await screen.findByRole("dialog");
    const confirmInput = await within(dialog).findByRole("textbox");
    fireEvent.change(confirmInput, { target: { value: "REMOVE /mnt/disk2" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Finish removal" }));

    expect(await within(dialog).findByText("threshold guard tripped")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("Cancel removal calls cancelDiskRemoval, and a rejected request is shown inside the dialog without closing it", async () => {
    mockPoolAndJobs(mockGet, poolWithRemovalState("evacuated"));
    mockPost.mockImplementation((path: string) => {
      if (path === "/disks/array/remove/cancel") {
        return Promise.resolve({ error: { message: "its removal state changed" }, response: { ok: false } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Cancel removal" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel removal" }));

    await waitFor(() =>
      expect(mockPost).toHaveBeenCalledWith("/disks/array/remove/cancel", expect.objectContaining({ body: { mountpoint: "/mnt/disk2" } })),
    );
    expect(await within(dialog).findByText("its removal state changed")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("does not close the Cancel removal dialog on Escape while the request is pending, and still shows its failure", async () => {
    mockPoolAndJobs(mockGet, poolWithRemovalState("evacuated"));
    const pendingPost: { release: (() => void) | null } = { release: null };
    mockPost.mockImplementation((path: string) => {
      if (path === "/disks/array/remove/cancel") {
        return new Promise((resolve) => {
          pendingPost.release = () => resolve({ error: { message: "its removal state changed" }, response: { ok: false } });
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderPool();

    fireEvent.click(await screen.findByRole("button", { name: "Cancel removal" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel removal" }));
    await waitFor(() => expect(within(dialog).getByRole("button", { name: /Cancel removal/ })).toBeDisabled());

    fireEvent.keyDown(document, { key: "Escape", code: "Escape" });
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    pendingPost.release?.();
    expect(await within(dialog).findByText("its removal state changed")).toBeInTheDocument();
  });

  it("a failed disk_remove job shows its message on the row, next to the Finish removal retry", async () => {
    mockPoolAndJobs(mockGet, poolWithRemovalState("unpooled"), [
      {
        id: "job-failed-1",
        type: "disk_remove",
        class: "topology",
        status: "failed",
        resumable: false,
        cancellable: false,
        createdAt: "2026-01-01T00:00:00Z",
        error: { code: "threshold_guard_tripped", message: "the threshold guard is tripped" },
      },
    ]);

    renderPool();

    expect(await screen.findByText(/the threshold guard is tripped/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Finish removal" })).toBeInTheDocument();
  });

  it("shows a failed disk_remove job newer than its own evacuation job — the current removal's own failure", async () => {
    mockPoolAndJobs(mockGet, poolWithRemovalState("unpooled"), [
      {
        id: "job-evac-1",
        type: "evacuation",
        class: "array_write",
        status: "succeeded",
        resumable: true,
        cancellable: true,
        createdAt: "2026-01-01T00:00:00Z",
      },
      {
        id: "job-failed-1",
        type: "disk_remove",
        class: "topology",
        status: "failed",
        resumable: false,
        cancellable: false,
        createdAt: "2026-01-02T00:00:00Z",
        error: { code: "threshold_guard_tripped", message: "the threshold guard is tripped" },
      },
    ]);

    renderPool();

    expect(await screen.findByText(/the threshold guard is tripped/)).toBeInTheDocument();
  });

  it("does not show a failed disk_remove job once a later disk_remove job for the same removal is queued or running", async () => {
    // Scenario: Finish removal failed, the user cleared the guard and
    // retried — the retry (job-remove-2) is running while the old
    // failure (job-remove-1) is still in the job list. The row must
    // reflect the retry in progress, not the superseded failure.
    mockPoolAndJobs(mockGet, poolWithRemovalState("unpooled"), [
      {
        id: "job-evac-1",
        type: "evacuation",
        class: "array_write",
        status: "succeeded",
        resumable: true,
        cancellable: true,
        createdAt: "2026-01-01T00:00:00Z",
      },
      {
        id: "job-remove-1",
        type: "disk_remove",
        class: "topology",
        status: "failed",
        resumable: false,
        cancellable: false,
        createdAt: "2026-01-02T00:00:00Z",
        error: { code: "threshold_guard_tripped", message: "the threshold guard is tripped" },
      },
      {
        id: "job-remove-2",
        type: "disk_remove",
        class: "topology",
        status: "running",
        resumable: false,
        cancellable: false,
        createdAt: "2026-01-03T00:00:00Z",
      },
    ]);

    renderPool();

    expect(await screen.findByRole("button", { name: "Finish removal" })).toBeInTheDocument();
    expect(screen.queryByText(/Finishing removal failed/)).not.toBeInTheDocument();
  });

  it("does not show a stale failed disk_remove job once a newer evacuation has started for a later removal (finding 2)", async () => {
    // Scenario: disk3's finish attempt failed, its retry later succeeded,
    // and disk2's own evacuation (shown as removalDisk here) only started
    // after that — the old disk3 failure must never be misattributed to
    // disk2's row.
    mockPoolAndJobs(mockGet, poolWithRemovalState("evacuated"), [
      {
        id: "job-failed-old",
        type: "disk_remove",
        class: "topology",
        status: "failed",
        resumable: false,
        cancellable: false,
        createdAt: "2026-01-01T00:00:00Z",
        error: { code: "threshold_guard_tripped", message: "the threshold guard is tripped" },
      },
      {
        id: "job-evac-new",
        type: "evacuation",
        class: "array_write",
        status: "succeeded",
        resumable: true,
        cancellable: true,
        createdAt: "2026-01-02T00:00:00Z",
      },
    ]);

    renderPool();

    expect(await screen.findByRole("button", { name: "Finish removal" })).toBeInTheDocument();
    expect(screen.queryByText(/Finishing removal failed/)).not.toBeInTheDocument();
    expect(
      screen.getByText("Takes /mnt/disk2 out of the pool and SnapRAID, then reports it safe to pull (doc 09 §4)."),
    ).toBeInTheDocument();
  });
});
