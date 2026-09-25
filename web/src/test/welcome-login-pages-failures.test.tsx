import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { LoginPage } from "@/routes/login";
import { WelcomePage } from "@/routes/welcome";

const mockGet = vi.fn();
const mockPost = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  },
}));

const mockRefresh = vi.fn();
const mockAcceptSession = vi.fn();

vi.mock("@/lib/api/auth-context", () => ({
  useAuth: () => ({
    phase: "welcome",
    user: null,
    adminExists: false,
    refresh: mockRefresh,
    acceptSession: mockAcceptSession,
  }),
}));

describe("Welcome and login pages load failures", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockRefresh.mockReset();
    mockAcceptSession.mockReset();
    sessionStorage.clear();
    localStorage.clear();
  });

  it("shows a doctor check error on the welcome page's system-check step when /doctor fails", async () => {
    sessionStorage.setItem("hoserva.onboardingStep", "1");
    mockGet.mockImplementation((path: string) => {
      if (path === "/doctor") {
        return Promise.resolve({ error: { message: "doctor check unavailable" }, response: { ok: false } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <WelcomePage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("doctor check unavailable")).toBeInTheDocument();
  });

  it("shows the login page's error when /auth/login fails", async () => {
    mockPost.mockImplementation((path: string) => {
      if (path === "/auth/login") {
        return Promise.resolve({ error: { message: "invalid username or password" }, response: { ok: false } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <LoginPage />
      </MemoryRouter>,
    );

    fireEvent.change(screen.getByRole("textbox", { name: "Username" }), { target: { value: "admin" } });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    expect(await screen.findByText("invalid username or password")).toBeInTheDocument();
    expect(mockAcceptSession).not.toHaveBeenCalled();
  });
});
