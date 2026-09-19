import { render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi, beforeEach } from "vitest";

import { App } from "@/App";

const mockGet = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: vi.fn(),
  },
}));

function mockMatchMedia(prefersDark: boolean): void {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    addEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
    matches: prefersDark,
    media: query,
    onchange: null,
    removeEventListener: vi.fn(),
  })) as unknown as typeof window.matchMedia;
}

describe("App shell", () => {
  beforeEach(() => {
    mockGet.mockReset();
    localStorage.clear();
    sessionStorage.clear();
    mockGet.mockImplementation((path: string) => {
      if (path === "/setup/status") {
        return Promise.resolve({ data: { adminExists: true }, response: { ok: true } });
      }
      if (path === "/auth/session") {
        return Promise.resolve({
          data: { id: "00000000-0000-0000-0000-000000000001", username: "admin", role: "admin", totpEnrolled: false },
          response: { ok: true },
        });
      }
      if (path === "/jobs") {
        return Promise.resolve({ data: { jobs: [] }, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
  });

  it("renders the sidebar navigation from the i18n catalog", async () => {
    mockMatchMedia(false);
    render(<App />);

    expect(await screen.findByText("Hoserva")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Dashboard" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Storage" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Jobs" })).toBeInTheDocument();
  });

  it("applies the dark class when the system prefers dark", async () => {
    mockMatchMedia(true);
    render(<App />);

    await waitFor(() => {
      expect(document.documentElement.classList.contains("dark")).toBe(true);
    });
  });

  it("does not apply the dark class when the system prefers light", async () => {
    mockMatchMedia(false);
    render(<App />);

    await waitFor(() => {
      expect(document.documentElement.classList.contains("dark")).toBe(false);
    });
  });
});
