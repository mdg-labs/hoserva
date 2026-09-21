import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
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

function share() {
  return {
    name: "media",
    path: "/mnt/user/media",
    cacheMode: "cache-then-move" as const,
    createPolicy: "mspmfs" as const,
    smb: { enabled: true, guest: false, readOnly: false, browseable: true, recycle: false, timeMachine: false },
    nfs: { enabled: false, hosts: [], squash: "root_squash" as const },
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
});
