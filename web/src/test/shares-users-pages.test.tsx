import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ShareDetailPage } from "@/routes/shares/detail";
import { SharesPage } from "@/routes/shares/index";
import { UsersPage } from "@/routes/users";

const mockGet = vi.fn();
const mockPost = vi.fn();
const mockPatch = vi.fn();
const mockPut = vi.fn();
const mockDelete = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
    PATCH: (...args: unknown[]) => mockPatch(...args),
    PUT: (...args: unknown[]) => mockPut(...args),
    DELETE: (...args: unknown[]) => mockDelete(...args),
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

function share(overrides: { usage?: { totalBytes: number; perDisk: { disk: string; bytes: number }[]; asOf: string } | null } = {}) {
  return {
    name: "media",
    path: "/mnt/user/media",
    cacheMode: "cache-then-move" as const,
    createPolicy: "mspmfs" as const,
    smb: { enabled: true, guest: false, readOnly: false, browseable: true, recycle: false, timeMachine: false },
    nfs: { enabled: false, hosts: [], squash: "root_squash" as const },
    usage: overrides.usage ?? null,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
  };
}

describe("SharesPage", () => {
  beforeEach(() => {
    cleanup();
    mockMatchMedia();
    mockGet.mockReset();
    mockPost.mockReset();
    mockDelete.mockReset();
  });

  it("shows the empty state teaching what a share is when none exist", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares") {
        return Promise.resolve({ data: { shares: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <SharesPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("No shares yet")).toBeInTheDocument();
  });

  it("lists a share's cache mode, SMB and NFS status", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares") {
        return Promise.resolve({ data: { shares: [share()] }, response: { ok: true } });
      }
      if (path === "/shares/{name}/permissions") {
        return Promise.resolve({ data: { users: [], groups: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <SharesPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("media")).toBeInTheDocument();
    expect(screen.getByText("Cache then move")).toBeInTheDocument();
  });

  it("shows an honest not-yet-synced state instead of a placeholder size", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares") {
        return Promise.resolve({ data: { shares: [share()] }, response: { ok: true } });
      }
      if (path === "/shares/{name}/permissions") {
        return Promise.resolve({ data: { users: [], groups: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <SharesPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("Not yet synced")).toBeInTheDocument();
  });

  it("renders a synced share's real size used", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares") {
        return Promise.resolve({
          data: {
            shares: [
              share({
                usage: {
                  totalBytes: 1_048_576,
                  perDisk: [{ disk: "/mnt/disk1", bytes: 1_048_576 }],
                  asOf: "2026-01-02T00:00:00Z",
                },
              }),
            ],
          },
          response: { ok: true },
        });
      }
      if (path === "/shares/{name}/permissions") {
        return Promise.resolve({ data: { users: [], groups: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <SharesPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("1.00 MiB")).toBeInTheDocument();
  });

  it("shows a maintenance-mode refusal inside the create dialog", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares") {
        return Promise.resolve({ data: { shares: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPost.mockResolvedValue({
      error: {
        code: "maintenance_mode",
        message: "maintenance mode is active — no new jobs are accepted",
      },
      response: { ok: false },
    });

    render(
      <MemoryRouter>
        <SharesPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("No shares yet")).toBeInTheDocument();
    const openButtons = screen.getAllByRole("button", { name: "Create share" });
    fireEvent.click(openButtons[0]);
    fireEvent.change(await screen.findByLabelText("Share name"), { target: { value: "hidden" } });
    const dialog = screen.getByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create share" }));

    expect(
      await within(dialog).findByText(/The array is stopped\. Start the array before creating/),
    ).toBeInTheDocument();
    expect(mockPost).toHaveBeenCalled();
  });
});

describe("ShareDetailPage danger zone", () => {
  beforeEach(() => {
    cleanup();
    mockMatchMedia();
    mockGet.mockReset();
    mockPost.mockReset();
    mockPatch.mockReset();
    mockPut.mockReset();
    mockDelete.mockReset();
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares/{name}") {
        return Promise.resolve({ data: share(), response: { ok: true } });
      }
      if (path === "/users") {
        return Promise.resolve({ data: { users: [] }, response: { ok: true } });
      }
      if (path === "/user-groups") {
        return Promise.resolve({ data: { groups: [] }, response: { ok: true } });
      }
      if (path === "/shares/{name}/permissions") {
        return Promise.resolve({ data: { users: [], groups: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
  });

  it("opens a plain confirm for removing the share definition", async () => {
    render(
      <MemoryRouter initialEntries={["/shares/media"]}>
        <Routes>
          <Route path="/shares/:name" element={<ShareDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("tab", { name: "Danger zone" }));
    fireEvent.click(await screen.findByRole("button", { name: "Remove definition" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Remove this share definition?")).toBeInTheDocument();
    expect(within(dialog).queryByLabelText("Type the confirmation phrase")).not.toBeInTheDocument();
  });

  it("requires a typed confirmation for deleting a share's data", async () => {
    render(
      <MemoryRouter initialEntries={["/shares/media"]}>
        <Routes>
          <Route path="/shares/:name" element={<ShareDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("tab", { name: "Danger zone" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete data" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByLabelText("Type the confirmation phrase")).toBeInTheDocument();
    const submit = within(dialog).getByRole("button", { name: "Delete data" });
    expect(submit).toBeDisabled();
  });

  it("warns that browsing may wake disks before the first listing", async () => {
    render(
      <MemoryRouter initialEntries={["/shares/media?tab=browse"]}>
        <Routes>
          <Route path="/shares/:name" element={<ShareDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Browsing may wake disks")).toBeInTheDocument();
    expect(
      mockGet.mock.calls.some((call) => call[0] === "/shares/{name}/browse"),
    ).toBe(false);
  });
});

describe("ShareDetailPage cache tab", () => {
  beforeEach(() => {
    cleanup();
    mockMatchMedia();
    mockGet.mockReset();
    mockPost.mockReset();
    mockPatch.mockReset();
    mockPut.mockReset();
    mockDelete.mockReset();
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares/{name}") {
        return Promise.resolve({
          data: {
            ...share(),
            cacheMode: "cache-then-move",
          },
          response: { ok: true },
        });
      }
      if (path === "/users") {
        return Promise.resolve({ data: { users: [] }, response: { ok: true } });
      }
      if (path === "/user-groups") {
        return Promise.resolve({ data: { groups: [] }, response: { ok: true } });
      }
      if (path === "/shares/{name}/permissions") {
        return Promise.resolve({ data: { users: [], groups: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
  });

  it("offers relocation when changing cache mode toward array-only", async () => {
    render(
      <MemoryRouter initialEntries={["/shares/media?tab=cache"]}>
        <Routes>
          <Route path="/shares/:name" element={<ShareDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("tab", { name: "Cache" }));
    fireEvent.click(screen.getByRole("radio", { name: /Array only/ }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Change mode and relocate")).toBeInTheDocument();
    expect(within(dialog).getByText("Change mode only")).toBeInTheDocument();
    expect(
      within(dialog).queryByText(/relocation job to move them is not available/i),
    ).not.toBeInTheDocument();
  });

  it("keeps the relocation dialog open when relocate fails after the mode is saved", async () => {
    mockPatch.mockResolvedValue({
      data: { ...share(), cacheMode: "array-only" },
      response: { ok: true },
    });
    mockPost.mockResolvedValue({
      error: { code: "job_conflict", message: "another array write job is running" },
      response: { ok: false },
    });

    render(
      <MemoryRouter initialEntries={["/shares/media?tab=cache"]}>
        <Routes>
          <Route path="/shares/:name" element={<ShareDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("tab", { name: "Cache" }));
    fireEvent.click(screen.getByRole("radio", { name: /Array only/ }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Change mode and relocate" }));

    expect(await within(dialog).findByText("another array write job is running")).toBeInTheDocument();
    expect(mockPatch).toHaveBeenCalled();
    expect(mockPost).toHaveBeenCalledWith("/shares/{name}/relocate", {
      params: { path: { name: "media" } },
      body: { to: "array" },
    });

    // The mode is already saved, but the files are still on the old
    // tier: the dialog must still offer the relocation to retry it.
    mockPost.mockResolvedValue({
      data: { id: "job-relocate-1" },
      response: { ok: true },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Change mode and relocate" }));
    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledTimes(2);
    });
    expect(mockPost).toHaveBeenLastCalledWith("/shares/{name}/relocate", {
      params: { path: { name: "media" } },
      body: { to: "array" },
    });
  });
});

describe("ShareDetailPage general tab", () => {
  beforeEach(() => {
    cleanup();
    mockMatchMedia();
    mockGet.mockReset();
    mockPost.mockReset();
    mockPatch.mockReset();
    mockPut.mockReset();
    mockDelete.mockReset();
  });

  function mockShareDetail(data: ReturnType<typeof share>): void {
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares/{name}") {
        return Promise.resolve({ data, response: { ok: true } });
      }
      if (path === "/users") {
        return Promise.resolve({ data: { users: [] }, response: { ok: true } });
      }
      if (path === "/user-groups") {
        return Promise.resolve({ data: { groups: [] }, response: { ok: true } });
      }
      if (path === "/shares/{name}/permissions") {
        return Promise.resolve({ data: { users: [], groups: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
  }

  it("reports an honest not-yet-synced state rather than a guessed size", async () => {
    mockShareDetail(share());

    render(
      <MemoryRouter initialEntries={["/shares/media"]}>
        <Routes>
          <Route path="/shares/:name" element={<ShareDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Not yet synced")).toBeInTheDocument();
  });

  it("renders the real size and per-disk distribution as of the last sync", async () => {
    mockShareDetail(
      share({
        usage: {
          totalBytes: 300,
          perDisk: [
            { disk: "/mnt/disk1", bytes: 100 },
            { disk: "/mnt/disk2", bytes: 200 },
          ],
          asOf: "2026-01-02T00:00:00Z",
        },
      }),
    );

    render(
      <MemoryRouter initialEntries={["/shares/media"]}>
        <Routes>
          <Route path="/shares/:name" element={<ShareDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    // The rendered "as of" text embeds a locale/timezone-formatted
    // timestamp, so only the size and the "as of" framing are pinned here.
    expect(await screen.findByText(/^Size used: 300 B \(as of .+\)$/)).toBeInTheDocument();
    expect(screen.getByText("/mnt/disk1")).toBeInTheDocument();
    expect(screen.getByText("/mnt/disk2")).toBeInTheDocument();
  });
});

describe("UsersPage", () => {
  beforeEach(() => {
    cleanup();
    mockMatchMedia();
    mockGet.mockReset();
    mockPost.mockReset();
    mockGet.mockImplementation((path: string) => {
      if (path === "/users") {
        return Promise.resolve({
          data: {
            users: [
              { id: "u1", username: "alice", role: "share-only", totpEnrolled: false, createdAt: "2026-01-01T00:00:00Z" },
            ],
          },
          response: { ok: true },
        });
      }
      if (path === "/user-groups") {
        return Promise.resolve({ data: { groups: [] }, response: { ok: true } });
      }
      if (path === "/sessions") {
        return Promise.resolve({ data: { sessions: [] }, response: { ok: true } });
      }
      if (path === "/api-tokens") {
        return Promise.resolve({ data: { tokens: [] }, response: { ok: true } });
      }
      if (path === "/shares") {
        return Promise.resolve({ data: { shares: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
  });

  it("makes UI login explicit per account", async () => {
    render(
      <MemoryRouter>
        <UsersPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("alice")).toBeInTheDocument();
    expect(screen.getByText("Share-only")).toBeInTheDocument();
    const noBadges = screen.getAllByText("No");
    expect(noBadges.length).toBeGreaterThan(0);
  });

  it("shows UI login and SMB access as explicit, separate controls on create", async () => {
    render(
      <MemoryRouter>
        <UsersPage />
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Create account" }));
    const panel = await screen.findByRole("dialog");
    expect(within(panel).getByText("Viewer")).toBeInTheDocument();
    expect(within(panel).getByText("Share-only")).toBeInTheDocument();
    expect(within(panel).getByText("SMB access")).toBeInTheDocument();
  });

  it("shows the edit panel's UI login and SMB access switches reflecting real current state", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/users") {
        return Promise.resolve({
          data: {
            users: [
              {
                id: "u1",
                username: "alice",
                role: "share-only",
                totpEnrolled: false,
                hasCredential: true,
                lastLogin: null,
                createdAt: "2026-01-01T00:00:00Z",
              },
            ],
          },
          response: { ok: true },
        });
      }
      if (path === "/user-groups") {
        return Promise.resolve({ data: { groups: [] }, response: { ok: true } });
      }
      if (path === "/sessions") {
        return Promise.resolve({ data: { sessions: [] }, response: { ok: true } });
      }
      if (path === "/api-tokens") {
        return Promise.resolve({ data: { tokens: [] }, response: { ok: true } });
      }
      if (path === "/shares") {
        return Promise.resolve({ data: { shares: [] }, response: { ok: true } });
      }
      if (path === "/users/{userId}/permissions") {
        return Promise.resolve({ data: { permissions: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <UsersPage />
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Actions for alice" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Edit" }));

    const panel = await screen.findByRole("dialog");
    // alice is share-only with a credential provisioned: SMB access reflects
    // that (on), UI login stays off since her role has no UI login at all —
    // both read from UserSummary's own hasCredential/role, not guessed.
    const uiLoginSwitch = within(panel).getByRole("switch", { name: "UI login" });
    const smbSwitch = within(panel).getByRole("switch", { name: "SMB access" });
    expect(uiLoginSwitch).toHaveAttribute("aria-checked", "false");
    expect(smbSwitch).toHaveAttribute("aria-checked", "true");
    expect(uiLoginSwitch).toHaveAttribute("aria-disabled", "true");
    expect(smbSwitch).toHaveAttribute("aria-disabled", "true");
    expect(within(panel).getByRole("checkbox", { name: "Set or change SMB password" })).toBeInTheDocument();
  });

  // #228 CodeRabbit finding: openEditPanel's permissions GET is unguarded,
  // so a slow response for a panel the admin has since closed and reopened
  // for a different user could overwrite that other user's draft. This
  // test fails against a version of openEditPanel that applies whichever
  // permissions response lands last, regardless of which user it was for.
  it("does not let a slow permissions response for one user overwrite another user's already-open edit panel", async () => {
    let resolveAlicePermissions: (value: { data: unknown; response: { ok: boolean } }) => void = () => {};
    const alicePermissionsPromise = new Promise<{ data: unknown; response: { ok: boolean } }>((resolve) => {
      resolveAlicePermissions = resolve;
    });

    mockGet.mockImplementation((path: string, options?: { params?: { path?: { userId?: string } } }) => {
      if (path === "/users") {
        return Promise.resolve({
          data: {
            users: [
              { id: "u1", username: "alice", role: "viewer", totpEnrolled: false, createdAt: "2026-01-01T00:00:00Z" },
              { id: "u2", username: "bob", role: "viewer", totpEnrolled: false, createdAt: "2026-01-01T00:00:00Z" },
            ],
          },
          response: { ok: true },
        });
      }
      if (path === "/user-groups") {
        return Promise.resolve({ data: { groups: [] }, response: { ok: true } });
      }
      if (path === "/sessions") {
        return Promise.resolve({ data: { sessions: [] }, response: { ok: true } });
      }
      if (path === "/api-tokens") {
        return Promise.resolve({ data: { tokens: [] }, response: { ok: true } });
      }
      if (path === "/shares") {
        return Promise.resolve({ data: { shares: [share()] }, response: { ok: true } });
      }
      if (path === "/users/{userId}/permissions") {
        const userId = options?.params?.path?.userId;
        if (userId === "u1") return alicePermissionsPromise;
        if (userId === "u2") {
          return Promise.resolve({
            data: { permissions: [{ shareName: "media", access: "read-write" }] },
            response: { ok: true },
          });
        }
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <UsersPage />
      </MemoryRouter>,
    );

    // Open alice's edit panel: her permissions GET fires and stays pending.
    fireEvent.click(await screen.findByRole("button", { name: "Actions for alice" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Edit" }));
    await screen.findByRole("dialog");

    // Close it and open bob's instead: his permissions GET resolves right away.
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(await screen.findByRole("button", { name: "Actions for bob" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Edit" }));
    const panel = await screen.findByRole("dialog");

    await waitFor(() => {
      expect(within(panel).getByRole("radio", { name: "Read-write" })).toHaveAttribute("aria-checked", "true");
    });

    // Alice's stale response lands now. It must not overwrite bob's panel —
    // give React a full flush (a real macrotask, not just a microtask) so a
    // buggy, unconditional setSharePermissionsDraft call has every chance
    // to apply before the assertion below checks it didn't.
    await act(async () => {
      resolveAlicePermissions({ data: { permissions: [{ shareName: "media", access: "read-only" }] }, response: { ok: true } });
      await new Promise((resolve) => setTimeout(resolve, 0));
    });

    expect(within(panel).getByRole("radio", { name: "Read-write" })).toHaveAttribute("aria-checked", "true");
    expect(within(panel).getByRole("radio", { name: "Read-only" })).toHaveAttribute("aria-checked", "false");
  });
});
