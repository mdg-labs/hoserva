import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { BackupSettingsPage } from "@/routes/settings/backup";
import { GeneralSettingsPage } from "@/routes/settings/general";
import { NetworkSettingsPage } from "@/routes/settings/network";
import { NotificationsSettingsPage } from "@/routes/settings/notifications";
import { SchedulesSettingsPage } from "@/routes/settings/schedules";
import { UpdatesSettingsPage } from "@/routes/settings/updates";

const mockGet = vi.fn();
const mockPost = vi.fn();
const mockPut = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
    PUT: (...args: unknown[]) => mockPut(...args),
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
    mockPut.mockReset();
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

  function backupApi(overrides: Record<string, unknown>): void {
    mockGet.mockImplementation((path: string) => {
      if (path in overrides) {
        return overrides[path];
      }
      if (path === "/backup/destinations") {
        return Promise.resolve({ data: { destinations: [] }, response: { ok: true } });
      }
      if (path === "/backup/drill") {
        return Promise.resolve({ data: {}, response: { ok: true } });
      }
      if (path === "/settings/general") {
        return Promise.resolve({ data: { backupPassphraseSet: true }, response: { ok: true } });
      }
      return notFound();
    });
  }

  it("shows a backup destinations error instead of an empty table when /backup/destinations fails", async () => {
    backupApi({
      "/backup/destinations": Promise.resolve({
        error: { code: "internal", message: "destinations unavailable" },
        response: { ok: false },
      }),
    });

    render(
      <MemoryRouter>
        <BackupSettingsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("Could not load backup destinations")).toBeInTheDocument();
    expect(await screen.findByText("destinations unavailable")).toBeInTheDocument();
    expect(screen.queryByText("No backup destinations yet")).not.toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("shows a restore drill error instead of 'never run' when /backup/drill fails", async () => {
    backupApi({
      "/backup/drill": Promise.resolve({
        error: { code: "internal", message: "drill unavailable" },
        response: { ok: false },
      }),
    });

    render(
      <MemoryRouter>
        <BackupSettingsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("Could not load the last restore drill")).toBeInTheDocument();
    expect(await screen.findByText("drill unavailable")).toBeInTheDocument();
    expect(screen.queryByText("Never run yet")).not.toBeInTheDocument();
  });

  it("names a daemon without a backup service instead of showing empty destinations or a never-run drill", async () => {
    const notConfigured = { error: { code: "not_configured", message: "no backup service" }, response: { ok: false } };
    backupApi({
      "/backup/destinations": Promise.resolve(notConfigured),
      "/backup/drill": Promise.resolve(notConfigured),
    });

    render(
      <MemoryRouter>
        <BackupSettingsPage />
      </MemoryRouter>,
    );

    expect(await screen.findAllByText("Backups are not available on this server")).toHaveLength(2);
    expect(screen.queryByText("No backup destinations yet")).not.toBeInTheDocument();
    expect(screen.queryByText("Never run yet")).not.toBeInTheDocument();
    expect(screen.queryByText("Could not load backup destinations")).not.toBeInTheDocument();
  });

  it("does not report the passphrase as unset when its status cannot be loaded", async () => {
    backupApi({
      "/settings/general": Promise.resolve({
        error: { code: "internal", message: "general unavailable" },
        response: { ok: false },
      }),
    });

    render(
      <MemoryRouter>
        <BackupSettingsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("Could not load the backup passphrase status")).toBeInTheDocument();
    expect(screen.queryByText("No backup passphrase is set.")).not.toBeInTheDocument();
    expect(screen.queryByText("A backup passphrase is set.")).not.toBeInTheDocument();
  });

  it("keeps the last drill result and names the failure when refreshing it fails", async () => {
    let drillCalls = 0;
    backupApi({});
    const base = mockGet.getMockImplementation() as (path: string) => unknown;
    mockGet.mockImplementation((path: string) => {
      if (path === "/backup/drill") {
        drillCalls += 1;
        if (drillCalls === 1) {
          return Promise.resolve({
            data: { lastRun: { ranAt: "2026-09-28T04:00:00Z", passed: true, destinations: [] } },
            response: { ok: true },
          });
        }
        return Promise.resolve({
          error: { code: "internal", message: "drill refresh unavailable" },
          response: { ok: false },
        });
      }
      return base(path);
    });

    render(
      <MemoryRouter>
        <BackupSettingsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("Passed")).toBeInTheDocument();
    expect(screen.queryByText("Could not load the last restore drill")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Refresh result" }));

    expect(await screen.findByText("Could not load the last restore drill")).toBeInTheDocument();
    expect(await screen.findByText("drill refresh unavailable")).toBeInTheDocument();
  });

  it("names the failure when the passphrase status cannot be refreshed after saving it", async () => {
    let generalCalls = 0;
    backupApi({});
    const base = mockGet.getMockImplementation() as (path: string) => unknown;
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/general") {
        generalCalls += 1;
        if (generalCalls === 1) {
          return Promise.resolve({ data: { backupPassphraseSet: false }, response: { ok: true } });
        }
        return Promise.resolve({
          error: { code: "internal", message: "general refresh unavailable" },
          response: { ok: false },
        });
      }
      return base(path);
    });
    mockPut.mockReturnValue(Promise.resolve({ data: { backupPassphraseSet: true }, response: { ok: true } }));

    render(
      <MemoryRouter>
        <BackupSettingsPage />
      </MemoryRouter>,
    );

    expect(await screen.findByText("No backup passphrase is set.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Set passphrase" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Passphrase"), { target: { value: "correct horse" } });
    fireEvent.change(within(dialog).getByLabelText("Repeat the passphrase"), { target: { value: "correct horse" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save passphrase" }));

    await waitFor(() => expect(mockPut).toHaveBeenCalled());
    expect(await screen.findByText("Could not load the backup passphrase status")).toBeInTheDocument();
    expect(await screen.findByText("general refresh unavailable")).toBeInTheDocument();
  });
});
