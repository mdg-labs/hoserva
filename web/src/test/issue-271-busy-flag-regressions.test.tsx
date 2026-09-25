// Regression tests for issue #271: a dialog or page whose "busy" indicator
// is only the OR of its mutations' `pending` flags drops that indicator
// while an awaited refresh (or a second mutation) between two steps of a
// handler is still in flight, letting Cancel — or a second click of the
// same action — through mid-handler.

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { CachePage } from "@/routes/storage/cache";
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

const mockRefresh = vi.fn(() => Promise.resolve());
const mockAcceptSession = vi.fn();

vi.mock("@/lib/api/auth-context", () => ({
  useAuth: () => ({
    phase: "authenticated",
    user: null,
    adminExists: false,
    refresh: mockRefresh,
    acceptSession: mockAcceptSession,
  }),
}));

vi.mock("@/hooks/use-system-status", () => ({
  useSystemData: () => ({
    status: null,
    pool: { disks: [] },
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

function resetMocks(): void {
  cleanup();
  mockMatchMedia();
  mockGet.mockReset();
  mockPost.mockReset();
  mockPatch.mockReset();
  mockPut.mockReset();
  mockDelete.mockReset();
  mockRefresh.mockReset();
  mockRefresh.mockImplementation(() => Promise.resolve());
  mockAcceptSession.mockReset();
  sessionStorage.clear();
  localStorage.clear();
}

// A click on a disabled <button> never runs its onClick handler, in jsdom
// as in a real browser — so asserting a mock call count didn't grow after
// firing a click on a "disabled" button is a meaningful check only once the
// element is confirmed disabled.
function click(el: Element): void {
  fireEvent.pointerDown(el, { button: 0, pointerId: 1 });
  fireEvent.pointerUp(el, { button: 0, pointerId: 1 });
  fireEvent.click(el);
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

describe("issue #271 — cache page's mode dialog stays busy across the whole handler", () => {
  beforeEach(resetMocks);

  // The dialog's busy flag was `cacheModeMutation.pending || relocateMutation.pending`,
  // which both go back to false as soon as the PATCH resolves — before
  // `saveShareCacheMode`'s own `await sharesQuery.refresh()` (the GET
  // /shares refetch) has landed. During that gap Cancel re-enabled, so an
  // operator who clicked it believed the change was cancelled while the
  // relocate that follows the refetch still queued a job (issue #271).
  it("keeps Cancel disabled while the post-patch /shares refetch is in flight, and does not relocate before it lands", async () => {
    let sharesCall = 0;
    const secondSharesFetch: { release: (() => void) | null } = { release: null };
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares") {
        sharesCall += 1;
        if (sharesCall === 1) {
          return Promise.resolve({ data: { shares: [share()] }, response: { ok: true } });
        }
        return new Promise((resolve) => {
          secondSharesFetch.release = () =>
            resolve({ data: { shares: [{ ...share(), cacheMode: "array-only" as const }] }, response: { ok: true } });
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPatch.mockImplementation((path: string) => {
      if (path === "/shares/{name}") {
        return Promise.resolve({ data: { ...share(), cacheMode: "array-only" as const }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPost.mockImplementation((path: string) => {
      if (path === "/shares/{name}/relocate") {
        return Promise.resolve({ data: { id: "job-1", class: "array_write", type: "relocate", status: "queued", createdAt: "2026-01-01T00:00:00Z" }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <CachePage />
      </MemoryRouter>,
    );

    const trigger = await screen.findByRole("combobox", { name: "Cache mode for media" });
    click(trigger);
    const option = await screen.findByRole("option", { name: "Array only" });
    click(option);

    const dialog = await screen.findByRole("dialog");
    const cancelButton = within(dialog).getByRole("button", { name: "Cancel" });
    expect(cancelButton).not.toBeDisabled();

    fireEvent.click(within(dialog).getByRole("button", { name: "Change mode and relocate" }));

    // The PATCH has resolved and the second GET /shares (the refetch) is
    // now the only thing still in flight.
    await waitFor(() => expect(sharesCall).toBe(2));
    await waitFor(() => expect(cancelButton).toBeDisabled());

    // Cancel is disabled, so clicking it must not close the dialog or fire
    // anything — and the relocate call must not have gone out yet, since
    // the refetch it's sequenced behind hasn't resolved.
    click(cancelButton);
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(mockPost).not.toHaveBeenCalledWith("/shares/{name}/relocate", expect.anything());

    secondSharesFetch.release?.();

    await waitFor(() =>
      expect(mockPost).toHaveBeenCalledWith(
        "/shares/{name}/relocate",
        expect.objectContaining({ params: { path: { name: "media" } } }),
      ),
    );
  });
});

describe("issue #271 — welcome page's admin step stays busy across the whole handler", () => {
  beforeEach(resetMocks);

  // `loading` was the OR of the six mutations' own `pending` flags.
  // setupAdminMutation's `pending` drops back to false as soon as
  // postSetupAdmin resolves — before handleAdminStep's own `await
  // refresh()` (the auth refresh) has landed and before `adminCreated` is
  // set. During that gap Next re-enabled, so a second click re-posted
  // /setup/admin (issue #271).
  it("keeps Next disabled while the post-admin-creation auth refresh is in flight, and posts /setup/admin only once", async () => {
    const authRefresh: { release: (() => void) | null } = { release: null };
    mockRefresh.mockImplementation(
      () =>
        new Promise<void>((resolve) => {
          authRefresh.release = resolve;
        }),
    );
    mockGet.mockImplementation((path: string) => {
      if (path === "/doctor") {
        return Promise.resolve({ data: { checks: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPost.mockImplementation((path: string) => {
      if (path === "/setup/admin") {
        return Promise.resolve({
          data: { id: "u1", username: "admin", role: "admin", totpEnrolled: false, createdAt: "2026-01-01T00:00:00Z" },
          response: { ok: true },
        });
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
    const nextButton = screen.getByRole("button", { name: "Next" });
    fireEvent.click(nextButton);

    await waitFor(() => expect(mockRefresh).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(nextButton).toBeDisabled());

    // Next is disabled, so a second click must not fire a second
    // /setup/admin request.
    click(nextButton);
    expect(mockPost.mock.calls.filter((call) => call[0] === "/setup/admin")).toHaveLength(1);

    authRefresh.release?.();

    await waitFor(() => expect(nextButton).not.toBeDisabled());
    expect(mockPost.mock.calls.filter((call) => call[0] === "/setup/admin")).toHaveLength(1);
  });
});
