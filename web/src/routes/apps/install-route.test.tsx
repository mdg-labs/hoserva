import { cleanup, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "@/App";

const mockGet = vi.fn();
const mockPost = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  },
}));

vi.mock("@/lib/api/auth-context", async (importOriginal) => {
  const original = await importOriginal<typeof import("@/lib/api/auth-context")>();
  return {
    ...original,
    useAuth: () => ({
      phase: "authenticated",
      user: { id: "1", username: "admin", role: "admin", totpEnrolled: false },
      adminExists: true,
      refresh: vi.fn(),
      acceptSession: vi.fn(),
    }),
  };
});

function ok<T>(data: T) {
  return Promise.resolve({ data, response: { ok: true } });
}

beforeEach(() => {
  cleanup();
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    addEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
    matches: false,
    media: query,
    onchange: null,
    removeEventListener: vi.fn(),
  })) as unknown as typeof window.matchMedia;
  mockGet.mockReset();
  mockPost.mockReset();
  mockGet.mockImplementation((path: string) => {
    switch (path) {
      case "/setup/status":
        return ok({ adminExists: true });
      case "/auth/session":
        return ok({ id: "1", username: "admin", role: "admin", totpEnrolled: false });
      case "/status":
        return ok({ healthy: true, summary: "OK", maintenanceMode: false });
      case "/catalog/{id}":
        return ok({
          id: "jellyfin",
          revision: 1,
          title: "Jellyfin",
          categories: ["media"],
          docs: "https://example.org/docs",
          source: "hoserva",
          sourceKind: "curated",
          signed: true,
          compose: "services: {}\n",
          privileges: [],
        });
      default:
        return Promise.resolve({ data: null, response: { ok: false } });
    }
  });
  mockPost.mockImplementation((path: string) =>
    path === "/templates/{id}/preview"
      ? ok({
          template: { source: "hoserva", id: "jellyfin", revision: "1" },
          title: "Jellyfin",
          name: "jellyfin",
          inputs: [],
          privileges: [],
          compose: "services: {}\n",
        })
      : Promise.resolve({ data: null, response: { ok: false } }),
  );
});

describe("the install route", () => {
  it("renders the install wizard at /apps/install/:appId, not a placeholder", async () => {
    window.history.pushState({}, "", "/apps/install/jellyfin");
    render(<App />);

    expect(await screen.findByRole("heading", { name: "Install Jellyfin" })).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Install Jellyfin" })).toBeInTheDocument();
    expect(screen.queryByText(/not available yet/i)).not.toBeInTheDocument();
    expect(mockPost).toHaveBeenCalledWith(
      "/templates/{id}/preview",
      expect.objectContaining({ params: { path: { id: "jellyfin" } } }),
    );
  });
});
