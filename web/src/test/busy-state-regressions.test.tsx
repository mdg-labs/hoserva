// Regression tests for issue #271's verified blocking findings. Each test
// documents the exact bug it would fail against on the parent (pre-fix)
// commit, per finding.

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ShareDetailPage } from "@/routes/shares/detail";
import { UsersPage } from "@/routes/users";
import { NotificationsSettingsPage } from "@/routes/settings/notifications";
import { SchedulesSettingsPage } from "@/routes/settings/schedules";
import { WelcomePage } from "@/routes/welcome";

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

function resetMocks(): void {
  cleanup();
  mockMatchMedia();
  mockGet.mockReset();
  mockPost.mockReset();
  mockPatch.mockReset();
  mockPut.mockReset();
  mockDelete.mockReset();
}

function share() {
  return {
    name: "media",
    path: "/mnt/user/media",
    cacheMode: "cache-then-move" as const,
    createPolicy: "mspmfs" as const,
    smb: { enabled: true, guest: false, readOnly: false, browseable: true, recycle: false, timeMachine: false },
    nfs: { enabled: false, hosts: [], squash: "root_squash" as const },
    usage: null,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
  };
}

describe("issue #271 finding 1 — welcome-flow mutations", () => {
  beforeEach(() => {
    resetMocks();
    sessionStorage.clear();
    localStorage.clear();
  });

  // handleAdminStep previously awaited postSetupAdmin in a try/finally with
  // no catch: a rejected promise (a dropped connection, not an API error)
  // was an unhandled rejection and the page showed nothing (issue #271
  // finding 1).
  it("shows an error instead of an unhandled rejection when admin creation rejects", async () => {
    mockPost.mockImplementation((path: string) => {
      if (path === "/setup/admin") {
        return Promise.reject(new Error("network down"));
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <WelcomePage />
      </MemoryRouter>,
    );

    fireEvent.change(await screen.findByRole("textbox", { name: "Username" }), { target: { value: "admin" } });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "correct horse battery staple" } });
    fireEvent.click(screen.getByRole("button", { name: "Next" }));

    expect(await screen.findByText("network down")).toBeInTheDocument();
  });

  // handleDoctorNext had the same try/finally-with-no-catch shape for
  // postDoctorHostConfig (issue #271 finding 1).
  it("shows an error instead of an unhandled rejection when the doctor host-config decision rejects", async () => {
    sessionStorage.setItem("hoserva.onboardingStep", "1");
    mockGet.mockImplementation((path: string) => {
      if (path === "/doctor") {
        return Promise.resolve({
          data: { checks: [{ id: "host_samba", status: "warn", message: "existing samba config found" }] },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPost.mockImplementation((path: string) => {
      if (path === "/doctor/host-config") {
        return Promise.reject(new Error("network down"));
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <WelcomePage />
      </MemoryRouter>,
    );

    await screen.findByText("existing samba config found");
    fireEvent.click(screen.getByRole("button", { name: "Next" }));

    expect(await screen.findByText("network down")).toBeInTheDocument();
  });
});

describe("issue #271 finding 2 — reopening a user's permissions panel", () => {
  beforeEach(resetMocks);

  // Fails against a version of the Save guard that checks
  // `permissionsQuery.loading`: reopening the panel for the same user keeps
  // the previous fetch's `data`, so `loading` (which requires
  // `data === null`) stays false throughout the refetch, and Save stays
  // enabled on the stale, since-reset-to-`{}` draft (issue #271 finding 2).
  // A save from that state would PUT `permissions: []`, wiping every grant
  // alice actually has.
  it("blocks Save until permissions are re-seeded for the current opening, not the previous one", async () => {
    const pendingFetch: { resolve: ((value: unknown) => void) | null } = { resolve: null };
    let permissionsCalls = 0;
    mockGet.mockImplementation((path: string) => {
      if (path === "/users") {
        return Promise.resolve({
          data: { users: [{ id: "u1", username: "alice", role: "viewer", totpEnrolled: false, createdAt: "2026-01-01T00:00:00Z" }] },
          response: { ok: true },
        });
      }
      if (path === "/user-groups") return Promise.resolve({ data: { groups: [] }, response: { ok: true } });
      if (path === "/sessions") return Promise.resolve({ data: { sessions: [] }, response: { ok: true } });
      if (path === "/api-tokens") return Promise.resolve({ data: { tokens: [] }, response: { ok: true } });
      if (path === "/shares") return Promise.resolve({ data: { shares: [share()] }, response: { ok: true } });
      if (path === "/users/{userId}/permissions") {
        permissionsCalls += 1;
        if (permissionsCalls === 1) {
          return Promise.resolve({
            data: { permissions: [{ shareName: "media", access: "read-write" }] },
            response: { ok: true },
          });
        }
        return new Promise((resolve) => {
          pendingFetch.resolve = resolve;
        });
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
    let panel = await screen.findByRole("dialog");
    await within(panel).findByRole("table");
    fireEvent.click(within(panel).getByRole("button", { name: "Cancel" }));

    fireEvent.click(await screen.findByRole("button", { name: "Actions for alice" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Edit" }));
    panel = await screen.findByRole("dialog");

    await waitFor(() => expect(permissionsCalls).toBe(2));
    expect(within(panel).getByRole("button", { name: "Save changes" })).toBeDisabled();

    pendingFetch.resolve?.({
      data: { permissions: [{ shareName: "media", access: "read-write" }] },
      response: { ok: true },
    });

    await waitFor(() => {
      expect(within(panel).getByRole("button", { name: "Save changes" })).not.toBeDisabled();
    });
    expect(mockPut).not.toHaveBeenCalledWith(
      "/users/{userId}/permissions",
      expect.objectContaining({ body: { permissions: [] } }),
    );
  });
});

describe("issue #271 finding 2 — the share detail permissions load guard", () => {
  beforeEach(resetMocks);

  // shares/detail.tsx already blocks the permissions Save button while
  // `permissionsLoadError` is set; this documents that guard so a future
  // change can't drop it silently (issue #271 finding 2).
  it("disables the permissions Save button when a share's permissions fail to load", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares/{name}") return Promise.resolve({ data: share(), response: { ok: true } });
      if (path === "/users") return Promise.resolve({ data: { users: [] }, response: { ok: true } });
      if (path === "/user-groups") return Promise.resolve({ data: { groups: [] }, response: { ok: true } });
      if (path === "/shares/{name}/permissions") {
        return Promise.resolve({
          error: { code: "internal", message: "permissions unavailable" },
          response: { ok: false },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter initialEntries={["/shares/media?tab=smb"]}>
        <Routes>
          <Route path="/shares/:name" element={<ShareDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    expect(await screen.findByText("permissions unavailable")).toBeInTheDocument();
    const saveButtons = screen.getAllByRole("button", { name: "Save changes" });
    expect(saveButtons[1]).toBeDisabled();
  });
});

describe("issue #271 finding 2 — permissions load failure on the users page", () => {
  beforeEach(resetMocks);

  // Fails against a version of openEditPanel that drops the permissions
  // fetch's error (users.tsx:176-184): the panel would show an empty,
  // silently-successful permissions matrix instead of an error, and Save
  // would go on to PUT `permissions: []`, wiping every grant this user
  // actually has.
  it("shows an error and disables Save when a user's permissions fail to load", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/users") {
        return Promise.resolve({
          data: { users: [{ id: "u1", username: "alice", role: "viewer", totpEnrolled: false, createdAt: "2026-01-01T00:00:00Z" }] },
          response: { ok: true },
        });
      }
      if (path === "/user-groups") return Promise.resolve({ data: { groups: [] }, response: { ok: true } });
      if (path === "/sessions") return Promise.resolve({ data: { sessions: [] }, response: { ok: true } });
      if (path === "/api-tokens") return Promise.resolve({ data: { tokens: [] }, response: { ok: true } });
      if (path === "/shares") return Promise.resolve({ data: { shares: [share()] }, response: { ok: true } });
      if (path === "/users/{userId}/permissions") {
        return Promise.resolve({ error: { code: "internal", message: "permissions unavailable" }, response: { ok: false } });
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

    expect(await within(panel).findByText("permissions unavailable")).toBeInTheDocument();
    expect(within(panel).queryByRole("table")).not.toBeInTheDocument();
    expect(within(panel).getByRole("button", { name: "Save changes" })).toBeDisabled();
  });

  // Fails against a version of syncGroupMembership that ignores
  // putUserGroupMembers' result (users.tsx:202): the panel would close as
  // if the save succeeded even though the membership change was dropped.
  it("keeps the edit panel open and shows an error when a group membership change fails to save", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/users") {
        return Promise.resolve({
          data: { users: [{ id: "u1", username: "alice", role: "viewer", totpEnrolled: false, createdAt: "2026-01-01T00:00:00Z" }] },
          response: { ok: true },
        });
      }
      if (path === "/user-groups") {
        return Promise.resolve({
          data: { groups: [{ id: "g1", name: "docs", memberUserIds: ["u1"] }] },
          response: { ok: true },
        });
      }
      if (path === "/sessions") return Promise.resolve({ data: { sessions: [] }, response: { ok: true } });
      if (path === "/api-tokens") return Promise.resolve({ data: { tokens: [] }, response: { ok: true } });
      if (path === "/shares") return Promise.resolve({ data: { shares: [] }, response: { ok: true } });
      if (path === "/users/{userId}/permissions") {
        return Promise.resolve({ data: { permissions: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPatch.mockResolvedValue({ data: { id: "u1", username: "alice", role: "viewer", totpEnrolled: false, createdAt: "2026-01-01T00:00:00Z" }, response: { ok: true } });
    mockPut.mockImplementation((path: string) => {
      if (path === "/user-groups/{groupId}/members") {
        return Promise.resolve({ error: { code: "internal", message: "membership save failed" }, response: { ok: false } });
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
    await within(panel).findByText("docs");

    fireEvent.click(within(panel).getByRole("button", { name: "Remove docs" }));
    fireEvent.click(within(panel).getByRole("button", { name: "Save changes" }));

    // The action banner renders at the page level, outside the sheet's own
    // content, so it's found in the whole document rather than within the
    // panel — but the panel itself stays open and mounted either way.
    expect(await screen.findByText("membership save failed")).toBeInTheDocument();
    expect(panel).toBeInTheDocument();
    expect(mockPut).toHaveBeenCalledWith(
      "/user-groups/{groupId}/members",
      expect.objectContaining({ params: { path: { groupId: "g1" } } }),
    );
  });
});

describe("issue #271 finding 3 — ambiguous non-OK empty-body responses on the share detail page", () => {
  beforeEach(() => {
    resetMocks();
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares/{name}") return Promise.resolve({ data: share(), response: { ok: true } });
      if (path === "/users") return Promise.resolve({ data: { users: [] }, response: { ok: true } });
      if (path === "/user-groups") return Promise.resolve({ data: { groups: [] }, response: { ok: true } });
      if (path === "/shares/{name}/permissions") {
        return Promise.resolve({ data: { users: [], groups: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
  });

  // openapi-fetch resolves a non-OK response with an empty body as
  // `{ data: undefined, error: undefined }`. A version of
  // handleRemoveDefinition that destructures `{ error }` alone reads that
  // as success and navigates away as if the share were actually removed
  // (issue #271 finding 3).
  it("does not navigate away when removing a share gets an empty non-OK response", async () => {
    mockDelete.mockResolvedValue({ data: undefined, error: undefined, response: { ok: false, status: 502 } });

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
    fireEvent.click(within(dialog).getByRole("button", { name: "Confirm" }));

    expect(await within(dialog).findByText("Request failed")).toBeInTheDocument();
    expect(dialog).toBeInTheDocument();
  });

  // handleDeleteData previously had only try/finally (no catch): a
  // rejected postShareDataDelete promise (a network failure, not an API
  // error) was an unhandled rejection and the dialog never showed
  // anything (issue #271 finding 3).
  it("shows an error instead of silently doing nothing when deleting data rejects", async () => {
    mockPost.mockImplementation((path: string) => {
      if (path === "/shares/{name}/data/delete") {
        return Promise.reject(new Error("network down"));
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

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
    fireEvent.change(within(dialog).getByLabelText("Type the confirmation phrase"), { target: { value: "media" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete data" }));

    expect(await screen.findByText("network down")).toBeInTheDocument();
  });
});

describe("issue #271 finding 4 — the welcome page's doctor check on unmount", () => {
  beforeEach(resetMocks);

  // This diff's AbortController had abort() in the effect's cleanup but no
  // .catch: any unmount during the /doctor request left an unhandled
  // AbortError (issue #271 finding 4). vitest fails the run on an
  // unhandled rejection, so this test simply has to survive the unmount.
  it("does not leave an unhandled rejection when unmounted while /doctor is in flight", async () => {
    sessionStorage.setItem("hoserva.onboardingStep", "1");
    const doctorCall = vi.fn((_path: string, options?: { signal?: AbortSignal }) => {
      return new Promise((_resolve, reject) => {
        options?.signal?.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")));
      });
    });
    mockGet.mockImplementation((path: string, options?: { signal?: AbortSignal }) => {
      if (path === "/doctor") return doctorCall(path, options);
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    const { unmount } = render(
      <MemoryRouter>
        <WelcomePage />
      </MemoryRouter>,
    );

    await waitFor(() => expect(doctorCall).toHaveBeenCalled());
    unmount();
    await new Promise((resolve) => setTimeout(resolve, 0));
    sessionStorage.clear();
  });
});

describe("issue #271 finding 5 — handlers called with void must not reject unhandled", () => {
  beforeEach(resetMocks);

  // handleChannelEnabled previously had no try around its await: a
  // rejected putNotificationChannel promise was an unhandled rejection and
  // the switch just snapped back with nothing shown (issue #271 finding 5).
  it("shows an error when toggling a notification channel rejects", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/notifications/channels") {
        return Promise.resolve({
          data: {
            channels: [
              {
                id: "c1",
                name: "Primary alerts",
                type: "email",
                enabled: true,
                hasSecret: true,
                createdAt: "2026-01-01T00:00:00Z",
                updatedAt: "2026-01-01T00:00:00Z",
              },
            ],
          },
          response: { ok: true },
        });
      }
      if (path === "/notifications/routing") return Promise.resolve({ data: { routing: [] }, response: { ok: true } });
      if (path === "/notifications/quiet-hours") {
        return Promise.resolve({
          data: { enabled: false, start: "22:00", end: "07:00", criticalAlwaysDelivers: true },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPut.mockImplementation((path: string) => {
      if (path === "/notifications/channels/{channelId}") {
        return Promise.reject(new Error("network down"));
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <NotificationsSettingsPage />
      </MemoryRouter>,
    );

    const toggle = await screen.findByRole("switch", { name: "Enabled" });
    fireEvent.click(toggle);

    expect(await screen.findByText("network down")).toBeInTheDocument();
  });

  // updateChainStep previously had no try around its await either, with the
  // same unhandled-rejection and silently-reverting-switch symptom (issue
  // #271 finding 5).
  it("shows an error when toggling a maintenance chain step rejects", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/schedules") {
        return Promise.resolve({
          data: {
            chain: {
              schedulePreview: "Nightly at 01:00",
              nextRun: "2026-01-02T01:00:00Z",
              steps: [{ id: "mover", enabled: true }],
            },
            otherJobs: [],
            conflicts: [],
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPut.mockImplementation((path: string) => {
      if (path === "/settings/schedules/chain") {
        return Promise.reject(new Error("network down"));
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <SchedulesSettingsPage />
      </MemoryRouter>,
    );

    const toggle = await screen.findByRole("switch", { name: /Mover/i });
    fireEvent.click(toggle);

    expect(await screen.findByText("network down")).toBeInTheDocument();
  });
});
