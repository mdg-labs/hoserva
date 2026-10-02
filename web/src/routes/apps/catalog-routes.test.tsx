import { cleanup, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "@/App";

const mockGet = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: vi.fn(),
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

const ENTRY = {
  id: "jellyfin",
  revision: 1,
  title: "Jellyfin",
  categories: ["media"],
  docs: "https://example.org/docs",
  source: "hoserva",
  sourceKind: "curated",
  signed: true,
  installed: false,
};

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
  mockGet.mockImplementation((path: string) => {
    switch (path) {
      case "/setup/status":
        return ok({ adminExists: true });
      case "/auth/session":
        return ok({ id: "1", username: "admin", role: "admin", totpEnrolled: false });
      case "/status":
        return ok({ healthy: true, summary: "OK", maintenanceMode: false });
      case "/catalog":
        return ok({ serial: 1, templates: [ENTRY] });
      case "/catalog/{id}":
        return ok({ ...ENTRY, compose: "services: {}\n", privileges: [] });
      default:
        return Promise.resolve({ data: null, response: { ok: false } });
    }
  });
});

describe("the catalog routes", () => {
  it("renders the catalog page at /apps/catalog from the application's route tree", async () => {
    window.history.pushState({}, "", "/apps/catalog");
    render(<App />);

    expect(await screen.findByRole("heading", { name: "App catalog" })).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "Jellyfin" })).toHaveAttribute("href", "/apps/catalog/jellyfin");
    expect(screen.queryByText(/not available yet/i)).not.toBeInTheDocument();
  });

  it("renders the app page at /apps/catalog/:appId, not a container's page", async () => {
    window.history.pushState({}, "", "/apps/catalog/jellyfin");
    render(<App />);

    expect(await screen.findByRole("heading", { name: "Jellyfin" })).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "Install" })).toHaveAttribute("href", "/apps/install/jellyfin");
    expect(mockGet).not.toHaveBeenCalledWith("/apps/{id}", expect.anything());
  });
});
