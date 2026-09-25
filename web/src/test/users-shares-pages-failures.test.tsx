import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { SharesPage } from "@/routes/shares/index";
import { ShareDetailPage } from "@/routes/shares/detail";
import { UsersPage } from "@/routes/users";

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

describe("Users and shares pages load failures", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      addEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
      matches: false,
      media: query,
      onchange: null,
      removeEventListener: vi.fn(),
    })) as unknown as typeof window.matchMedia;
  });

  it("shows a users list error instead of the empty state when /users fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/users") {
        return Promise.resolve({ error: { message: "users unavailable" }, response: { ok: false } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <UsersPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("users unavailable")).toBeInTheDocument();
    expect(screen.queryByText("No accounts yet")).not.toBeInTheDocument();
  });

  it("shows a shares list error instead of the empty state when /shares fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares") {
        return Promise.resolve({ error: { message: "shares unavailable" }, response: { ok: false } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <SharesPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("shares unavailable")).toBeInTheDocument();
    expect(screen.queryByText("No shares yet")).not.toBeInTheDocument();
  });

  it("shows a share detail error instead of the not-found state when /shares/{name} fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/shares/{name}") {
        return Promise.resolve({ error: { message: "share unavailable" }, response: { ok: false } });
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

    expect(await screen.findByText("share unavailable")).toBeInTheDocument();
    expect(screen.queryByText("Share media was not found.")).not.toBeInTheDocument();
  });
});
