// Regression tests for issue #375: the cache-mode dialogs on
// /storage/cache and /shares/:name could be dismissed with Escape or a
// backdrop click while their save/relocate handler was still running, and
// several action errors rendered in a page-level banner hidden behind an
// open panel or overlay instead of inside it.
//
// Regression tests for issue #376, the same two patterns on the remaining
// /shares/:name dialogs and the /users group/token creation overlays.

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ShareDetailPage } from "@/routes/shares/detail";
import { CachePage } from "@/routes/storage/cache";
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

vi.mock("@/hooks/use-system-status", () => ({
  useSystemData: () => ({
    status: null,
    pool: {
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
    jobs: [],
    doctor: null,
    loading: false,
    error: null,
    refresh: vi.fn(() => Promise.resolve()),
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

function click(el: Element): void {
  fireEvent.pointerDown(el, { button: 0, pointerId: 1 });
  fireEvent.pointerUp(el, { button: 0, pointerId: 1 });
  fireEvent.click(el);
}

function share(overrides: { cacheMode?: "cache-then-move" | "cache-only" | "array-only" } = {}) {
  return {
    name: "media",
    path: "/mnt/user/media",
    cacheMode: overrides.cacheMode ?? "cache-only",
    createPolicy: "mspmfs" as const,
    smb: { enabled: true, guest: false, readOnly: false, browseable: true, recycle: false, timeMachine: false },
    nfs: { enabled: false, hosts: [], squash: "root_squash" as const },
    usage: null,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
  };
}

function resetMocks(): void {
  cleanup();
  mockMatchMedia();
  mockGet.mockReset();
  mockPost.mockReset();
  mockPatch.mockReset();
  mockPut.mockReset();
  mockDelete.mockReset();
  mockDelete.mockImplementation(() => Promise.resolve({ data: null, response: { ok: false } }));
}

function mockUsersData(): void {
  mockGet.mockImplementation((path: string) => {
    if (path === "/users") {
      return Promise.resolve({
        data: {
          users: [
            { id: "u1", username: "alice", role: "viewer", totpEnrolled: false, createdAt: "2026-01-01T00:00:00Z" },
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
}

describe("issue #375 — cache-mode dialogs ignore Escape while busy", () => {
  beforeEach(resetMocks);

  it("on /storage/cache: does not close the mode dialog on Escape while the patch is in flight", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares") {
        return Promise.resolve({ data: { shares: [share()] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    const pendingPatch: { release: (() => void) | null } = { release: null };
    mockPatch.mockImplementation(
      () =>
        new Promise((resolve) => {
          pendingPatch.release = () =>
            resolve({ data: { ...share(), cacheMode: "cache-then-move" }, response: { ok: true } });
        }),
    );

    render(
      <MemoryRouter>
        <CachePage />
      </MemoryRouter>,
    );

    const trigger = await screen.findByRole("combobox", { name: "Cache mode for media" });
    click(trigger);
    const option = await screen.findByRole("option", { name: "Cache then move" });
    click(option);

    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Change mode only" }));

    await waitFor(() => expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeDisabled());

    fireEvent.keyDown(document, { key: "Escape", code: "Escape" });

    // Still open, and still busy — a real dismiss would have unmounted the
    // dialog and its Cancel button entirely.
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeDisabled();

    pendingPatch.release?.();
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("on /shares/:name: does not close the cache-mode dialog on Escape while the patch is in flight", async () => {
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
    const pendingPatch: { release: (() => void) | null } = { release: null };
    mockPatch.mockImplementation(
      () =>
        new Promise((resolve) => {
          pendingPatch.release = () =>
            resolve({ data: { ...share(), cacheMode: "cache-then-move" }, response: { ok: true } });
        }),
    );

    render(
      <MemoryRouter initialEntries={["/shares/media?tab=cache"]}>
        <Routes>
          <Route path="/shares/:name" element={<ShareDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("tab", { name: "Cache" }));
    fireEvent.click(screen.getByRole("radio", { name: /Cache then move/ }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Change mode only" }));

    await waitFor(() => expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeDisabled());

    fireEvent.keyDown(document, { key: "Escape", code: "Escape" });

    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeDisabled();

    pendingPatch.release?.();
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });
});

describe("issue #375 — share detail delete-data overlay shows its own error", () => {
  beforeEach(resetMocks);

  it("keeps the overlay open with the typed name and shows the error inside it, not the page banner", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares/{name}") {
        return Promise.resolve({ data: share({ cacheMode: "cache-then-move" }), response: { ok: true } });
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
    mockPost.mockImplementation((path: string) => {
      if (path === "/shares/{name}/data/delete") {
        return Promise.resolve({
          error: { code: "internal_error", message: "delete failed: disk busy" },
          response: { ok: false },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter initialEntries={["/shares/media?tab=danger"]}>
        <Routes>
          <Route path="/shares/:name" element={<ShareDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("tab", { name: "Danger zone" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete data" }));

    const dialog = await screen.findByRole("dialog");
    const input = within(dialog).getByRole("textbox", { name: "Type the confirmation phrase" });
    fireEvent.change(input, { target: { value: "media" } });

    fireEvent.click(within(dialog).getByRole("button", { name: "Delete data" }));

    expect(await within(dialog).findByText("delete failed: disk busy")).toBeInTheDocument();
    // Still open, with the typed name kept, so the operator does not have
    // to retype it to retry.
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(input).toHaveValue("media");
    // The error is not duplicated as a page-level banner behind the overlay.
    expect(screen.getAllByText("delete failed: disk busy")).toHaveLength(1);
  });
});

describe("issue #375 — users panel shows its own action errors", () => {
  beforeEach(resetMocks);

  it("shows a role-save failure inside the open edit panel, not the page banner", async () => {
    mockUsersData();
    mockPatch.mockImplementation((path: string) => {
      if (path === "/users/{userId}") {
        return Promise.resolve({
          error: { code: "internal_error", message: "role save failed" },
          response: { ok: false },
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
    const panel = await screen.findByRole("dialog");

    fireEvent.click(within(panel).getByRole("button", { name: "Save changes" }));

    expect(await within(panel).findByText("role save failed")).toBeInTheDocument();
    // Exactly one render of the message: inside the panel, never duplicated
    // as the page-level banner behind it.
    expect(screen.getAllByText("role save failed")).toHaveLength(1);
  });

  it("shows a permissions-save failure inside the open edit panel, not the page banner", async () => {
    mockUsersData();
    mockPatch.mockImplementation((path: string) => {
      if (path === "/users/{userId}") {
        return Promise.resolve({ data: { id: "u1", username: "alice", role: "viewer", totpEnrolled: false }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPut.mockImplementation((path: string) => {
      if (path === "/users/{userId}/permissions") {
        return Promise.resolve({
          error: { code: "internal_error", message: "permissions save failed" },
          response: { ok: false },
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
    const panel = await screen.findByRole("dialog");

    fireEvent.click(within(panel).getByRole("button", { name: "Save changes" }));

    expect(await within(panel).findByText("permissions save failed")).toBeInTheDocument();
    expect(screen.getAllByText("permissions save failed")).toHaveLength(1);
  });
});

describe("issue #376 — remaining share-detail dialogs ignore Escape while busy", () => {
  beforeEach(resetMocks);

  function mockShareDetailData(): void {
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares/{name}") {
        return Promise.resolve({ data: share({ cacheMode: "cache-then-move" }), response: { ok: true } });
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

  it("does not close the remove-share-definition dialog on Escape while the delete request is pending", async () => {
    mockShareDetailData();
    const pendingDelete: { release: (() => void) | null } = { release: null };
    mockDelete.mockImplementation((path: string) => {
      if (path === "/shares/{name}") {
        return new Promise((resolve) => {
          pendingDelete.release = () => resolve({ data: null, response: { ok: true } });
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter initialEntries={["/shares/media?tab=danger"]}>
        <Routes>
          <Route path="/shares/:name" element={<ShareDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("tab", { name: "Danger zone" }));
    fireEvent.click(await screen.findByRole("button", { name: "Remove definition" }));

    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Confirm" }));

    // The busy button's accessible name gains the spinner's "Loading" text
    // once it's disabled, so match it loosely from here on.
    await waitFor(() => expect(within(dialog).getByRole("button", { name: /Confirm/ })).toBeDisabled());

    fireEvent.keyDown(document, { key: "Escape", code: "Escape" });

    // Still open, and still busy — a real dismiss would have unmounted the
    // dialog and its Confirm button entirely.
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: /Confirm/ })).toBeDisabled();

    pendingDelete.release?.();
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("does not close the delete-data overlay on Escape while the delete request is pending", async () => {
    mockShareDetailData();
    const pendingPost: { release: (() => void) | null } = { release: null };
    mockPost.mockImplementation((path: string) => {
      if (path === "/shares/{name}/data/delete") {
        return new Promise((resolve) => {
          pendingPost.release = () => resolve({ data: null, response: { ok: true } });
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter initialEntries={["/shares/media?tab=danger"]}>
        <Routes>
          <Route path="/shares/:name" element={<ShareDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("tab", { name: "Danger zone" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete data" }));

    const dialog = await screen.findByRole("dialog");
    const input = within(dialog).getByRole("textbox", { name: "Type the confirmation phrase" });
    fireEvent.change(input, { target: { value: "media" } });

    fireEvent.click(within(dialog).getByRole("button", { name: "Delete data" }));

    // Same spinner-name caveat as the remove-share-definition dialog above.
    await waitFor(() => expect(within(dialog).getByRole("button", { name: /Delete data/ })).toBeDisabled());

    fireEvent.keyDown(document, { key: "Escape", code: "Escape" });

    // Still open and still busy — a real dismiss would have unmounted the
    // overlay and its typed confirmation with it.
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(input).toHaveValue("media");

    pendingPost.release?.();
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });
});

describe("issue #376 — users group/token creation overlays show their own errors", () => {
  beforeEach(resetMocks);

  it("shows a failed group creation inside its open overlay, which stays open", async () => {
    mockUsersData();
    mockPost.mockImplementation((path: string) => {
      if (path === "/user-groups") {
        return Promise.resolve({
          error: { code: "internal_error", message: "group create failed" },
          response: { ok: false },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <UsersPage />
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Create group" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Group name" }), {
      target: { value: "movies" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create group" }));

    expect(await within(dialog).findByText("group create failed")).toBeInTheDocument();
    // Still open, and the error is not duplicated as a page-level banner
    // behind the overlay.
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(screen.getAllByText("group create failed")).toHaveLength(1);
  });

  it("shows a failed API-token creation inside its open overlay, which stays open", async () => {
    mockUsersData();
    mockPost.mockImplementation((path: string) => {
      if (path === "/users/{username}/tokens") {
        return Promise.resolve({
          error: { code: "internal_error", message: "token create failed" },
          response: { ok: false },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <UsersPage />
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Create token" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Account" }));
    fireEvent.click(await screen.findByRole("option", { name: "alice" }));
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Token name" }), {
      target: { value: "backup-script" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create token" }));

    expect(await within(dialog).findByText("token create failed")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(screen.getAllByText("token create failed")).toHaveLength(1);
  });

  it("does not close the group creation overlay on Escape while the create request is pending, and still shows its failure", async () => {
    mockUsersData();
    const pendingPost: { release: (() => void) | null } = { release: null };
    mockPost.mockImplementation((path: string) => {
      if (path === "/user-groups") {
        return new Promise((resolve) => {
          pendingPost.release = () =>
            resolve({ error: { code: "internal_error", message: "group create failed" }, response: { ok: false } });
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <UsersPage />
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Create group" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Group name" }), {
      target: { value: "movies" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create group" }));

    await waitFor(() => expect(within(dialog).getByRole("button", { name: /Create group/ })).toBeDisabled());

    fireEvent.keyDown(document, { key: "Escape", code: "Escape" });

    // Still open and still busy — a real dismiss would have unmounted the
    // overlay before the failure had anywhere to render.
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: /Create group/ })).toBeDisabled();

    pendingPost.release?.();
    expect(await within(dialog).findByText("group create failed")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("does not close the token creation overlay on Escape while the create request is pending, and still shows its failure", async () => {
    mockUsersData();
    const pendingPost: { release: (() => void) | null } = { release: null };
    mockPost.mockImplementation((path: string) => {
      if (path === "/users/{username}/tokens") {
        return new Promise((resolve) => {
          pendingPost.release = () =>
            resolve({ error: { code: "internal_error", message: "token create failed" }, response: { ok: false } });
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <UsersPage />
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Create token" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Account" }));
    fireEvent.click(await screen.findByRole("option", { name: "alice" }));
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Token name" }), {
      target: { value: "backup-script" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create token" }));

    await waitFor(() => expect(within(dialog).getByRole("button", { name: /Create token/ })).toBeDisabled());

    fireEvent.keyDown(document, { key: "Escape", code: "Escape" });

    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: /Create token/ })).toBeDisabled();

    pendingPost.release?.();
    expect(await within(dialog).findByText("token create failed")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });
});
