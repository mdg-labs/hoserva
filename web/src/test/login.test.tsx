import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi, beforeEach } from "vitest";

import { AuthProvider } from "@/lib/api/auth-guard";
import { LoginPage } from "@/routes/login";

const mockGet = vi.fn();
const mockPost = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  },
}));

function renderLogin(): ReturnType<typeof render> {
  return render(
    <MemoryRouter>
      <AuthProvider>
        <LoginPage />
      </AuthProvider>
    </MemoryRouter>,
  );
}

describe("LoginPage", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockGet.mockImplementation((path: string) => {
      if (path === "/setup/status") {
        return Promise.resolve({ data: { adminExists: true }, response: { ok: true } });
      }
      if (path === "/auth/session") {
        return Promise.resolve({ data: null, response: { ok: false } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
  });

  it("shows the TOTP field when login returns totp_required", async () => {
    mockPost.mockResolvedValueOnce({
      error: { code: "totp_required", message: "Authenticator code required" },
      response: { ok: false },
    });

    renderLogin();
    await waitFor(() => expect(screen.getByLabelText("Username")).toBeInTheDocument());

    const user = { type: (el: Element, text: string) => fireEvent.change(el, { target: { value: text } }) };
    await user.type(screen.getByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    expect(await screen.findByText("This account uses an authenticator — enter the current code.")).toBeInTheDocument();
    expect(screen.getByLabelText("Authentication code")).toBeInTheDocument();
  });

  it("shows an invalid-code message when login returns totp_invalid", async () => {
    mockPost.mockResolvedValueOnce({
      error: { code: "totp_invalid", message: "Invalid authenticator code" },
      response: { ok: false },
    });

    renderLogin();
    await waitFor(() => expect(screen.getByLabelText("Username")).toBeInTheDocument());

    const user = { type: (el: Element, text: string) => fireEvent.change(el, { target: { value: text } }) };
    await user.type(screen.getByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    expect(await screen.findByText("That authenticator code is not valid. Try again.")).toBeInTheDocument();
  });

  it("shows the lockout message when login returns rate_limited", async () => {
    mockPost.mockResolvedValueOnce({
      error: {
        code: "rate_limited",
        message: "Try again in 15 minutes.",
      },
      response: { ok: false, status: 429 },
    });

    renderLogin();
    await waitFor(() => expect(screen.getByLabelText("Username")).toBeInTheDocument());

    fireEvent.change(screen.getByLabelText("Username"), { target: { value: "admin" } });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "wrong-password" } });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    expect(
      await screen.findByText("Too many failed attempts. Try again in 15 minutes."),
    ).toBeInTheDocument();
  });
});
