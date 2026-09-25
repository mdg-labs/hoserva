import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { GeneralSettingsPage } from "@/routes/settings/general";
import { NetworkSettingsPage } from "@/routes/settings/network";
import { NotificationsSettingsPage } from "@/routes/settings/notifications";
import { SchedulesSettingsPage } from "@/routes/settings/schedules";
import { UpdatesSettingsPage } from "@/routes/settings/updates";

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

function notFound() {
  return Promise.resolve({ data: null, response: { ok: false } });
}

describe("Settings pages load failures", () => {
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

  it("shows a general settings error when /settings/general fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/general") {
        return Promise.resolve({ error: { message: "general settings unavailable" }, response: { ok: false } });
      }
      if (path === "/settings/ups") {
        return Promise.resolve({ data: { configured: false }, response: { ok: true } });
      }
      return notFound();
    });

    render(
      <MemoryRouter>
        <GeneralSettingsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("general settings unavailable")).toBeInTheDocument();
  });

  it("shows a network settings error instead of the interfaces table when /settings/network fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/network") {
        return Promise.resolve({ error: { message: "network settings unavailable" }, response: { ok: false } });
      }
      return notFound();
    });

    render(
      <MemoryRouter>
        <NetworkSettingsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("network settings unavailable")).toBeInTheDocument();
  });

  it("shows a notifications settings error when /notifications/channels fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/notifications/channels") {
        return Promise.resolve({ error: { message: "channels unavailable" }, response: { ok: false } });
      }
      if (path === "/notifications/routing") {
        return Promise.resolve({ data: { routing: [] }, response: { ok: true } });
      }
      if (path === "/notifications/quiet-hours") {
        return Promise.resolve({
          data: { enabled: false, start: "22:00", end: "07:00", criticalAlwaysDelivers: true },
          response: { ok: true },
        });
      }
      return notFound();
    });

    render(
      <MemoryRouter>
        <NotificationsSettingsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("channels unavailable")).toBeInTheDocument();
  });

  it("shows an updates settings error when /settings/updates fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/updates") {
        return Promise.resolve({ error: { message: "update status unavailable" }, response: { ok: false } });
      }
      return notFound();
    });

    render(
      <MemoryRouter>
        <UpdatesSettingsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("update status unavailable")).toBeInTheDocument();
  });

  it("shows a schedules load error instead of the chain settings when /settings/schedules fails", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/schedules") {
        return Promise.resolve({ error: { message: "schedules unavailable" }, response: { ok: false } });
      }
      return notFound();
    });

    render(
      <MemoryRouter>
        <SchedulesSettingsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("Could not load schedules")).toBeInTheDocument();
    expect(await screen.findByText("schedules unavailable")).toBeInTheDocument();
    expect(screen.queryByText("Nightly maintenance chain")).not.toBeInTheDocument();
  });
});
