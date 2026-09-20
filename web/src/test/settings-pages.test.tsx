import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi, beforeEach } from "vitest";

import { ToastProvider } from "@/components/ui/toast";
import { GeneralSettingsPage } from "@/routes/settings/general";
import { NotificationsSettingsPage } from "@/routes/settings/notifications";
import { SchedulesSettingsPage } from "@/routes/settings/schedules";
import { UpdatesSettingsPage } from "@/routes/settings/updates";
import { MAINTENANCE_CHAIN_STEPS } from "@/lib/maintenance-chain";

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

function renderWithToast(ui: React.ReactElement): ReturnType<typeof render> {
  return render(
    <ToastProvider>
      <MemoryRouter>{ui}</MemoryRouter>
    </ToastProvider>,
  );
}

describe("Settings pages", () => {
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

  it("loads and saves general hostname and timezone", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/general") {
        return Promise.resolve({
          data: { hostname: "nas", timezone: "Europe/Berlin", backupPassphraseSet: false },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPut.mockResolvedValue({
      data: { hostname: "hoserva", timezone: "UTC", backupPassphraseSet: false },
      response: { ok: true },
    });

    renderWithToast(<GeneralSettingsPage />);

    const hostnameInput = await screen.findByDisplayValue("nas");
    fireEvent.change(hostnameInput, { target: { value: "hoserva" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => {
      expect(mockPut).toHaveBeenCalledWith("/settings/general", {
        body: {
          hostname: "hoserva",
          timezone: "Europe/Berlin",
        },
      });
    });
  });

  it("loads notifications data and can trigger a test send", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/notifications/channels") {
        return Promise.resolve({
          data: {
            channels: [
              {
                id: "11111111-1111-4111-8111-111111111111",
                name: "Primary alerts",
                type: "email",
                enabled: true,
                hasSecret: true,
                createdAt: "2026-01-01T00:00:00Z",
                updatedAt: "2026-01-01T00:00:00Z",
              },
            ],
          },
          response: { ok: true },
        });
      }
      if (path === "/notifications/routing") {
        return Promise.resolve({
          data: {
            routing: [
              {
                eventType: "array_degraded",
                severity: "critical",
                channelIds: ["11111111-1111-4111-8111-111111111111"],
              },
            ],
          },
          response: { ok: true },
        });
      }
      if (path === "/notifications/quiet-hours") {
        return Promise.resolve({
          data: {
            enabled: false,
            start: "22:00",
            end: "07:00",
            criticalAlwaysDelivers: true,
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPost.mockResolvedValue({
      data: { success: true },
      response: { ok: true },
    });

    renderWithToast(<NotificationsSettingsPage />);

    expect(await screen.findByRole("button", { name: "Send test notification" })).toBeInTheDocument();
    expect(screen.getByText("Event routing")).toBeInTheDocument();
    expect(screen.getByText("Quiet hours")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Send test notification" }));

    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledWith("/notifications/channels/{channelId}/test", {
        params: { path: { channelId: "11111111-1111-4111-8111-111111111111" } },
      });
    });
  });

  it("renders the Q30 maintenance chain without a reorder control", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/schedules") {
        return Promise.resolve({
          data: {
            chain: {
              startTime: "02:00",
              weeklyScrubDay: 0,
              schedulePreview: "every day at 02:00",
              nextRun: "2026-06-16T00:00:00.000Z",
              steps: [
                { id: "mover", enabled: true },
                { id: "diff_guard", enabled: true },
                { id: "sync", enabled: true },
                { id: "scrub", enabled: true },
                { id: "config_backup", enabled: true },
              ],
            },
            otherJobs: [
              {
                id: "smart_self_test",
                enabled: true,
                frequency: "weekly",
                time: "03:00",
                schedulePreview: "every week at 03:00",
                nextRun: "2026-06-16T01:00:00.000Z",
              },
              {
                id: "appdata_backup",
                enabled: false,
                frequency: "daily",
                time: "04:00",
                schedulePreview: "every day at 04:00",
                nextRun: "2026-06-16T02:00:00.000Z",
              },
              {
                id: "restore_drill",
                enabled: false,
                frequency: "monthly",
                time: "05:00",
                schedulePreview: "every month at 05:00",
                nextRun: "2026-07-16T03:00:00.000Z",
              },
              {
                id: "container_update_check",
                enabled: true,
                frequency: "daily",
                time: "06:00",
                schedulePreview: "every day at 06:00",
                nextRun: "2026-06-16T04:00:00.000Z",
              },
            ],
            conflicts: [],
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderWithToast(<SchedulesSettingsPage />);

    expect(await screen.findByText("Nightly maintenance chain")).toBeInTheDocument();
    expect(MAINTENANCE_CHAIN_STEPS).toEqual([
      "mover",
      "diff_guard",
      "sync",
      "scrub",
      "config_backup",
    ]);
    expect(screen.getByText("Mover")).toBeInTheDocument();
    expect(screen.getByText("Diff and threshold guard")).toBeInTheDocument();
    expect(screen.getByText("Parity sync")).toBeInTheDocument();
    expect(screen.getByText("Scrub")).toBeInTheDocument();
    expect(screen.getByText("Config backup")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /reorder/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /move up/i })).not.toBeInTheDocument();
  });

  it("persists other-job time on blur, not on each change", async () => {
    const schedulesPayload = {
      chain: {
        startTime: "02:00",
        weeklyScrubDay: 0,
        schedulePreview: "every day at 02:00",
        nextRun: "2026-06-16T00:00:00.000Z",
        steps: [
          { id: "mover", enabled: true },
          { id: "diff_guard", enabled: true },
          { id: "sync", enabled: true },
          { id: "scrub", enabled: true },
          { id: "config_backup", enabled: true },
        ],
      },
      otherJobs: [
        {
          id: "smart_self_test",
          enabled: true,
          frequency: "weekly",
          time: "03:00",
          schedulePreview: "every Sunday at 03:00",
          nextRun: "2026-06-21T01:00:00.000Z",
        },
        {
          id: "appdata_backup",
          enabled: false,
          frequency: "daily",
          time: "04:00",
          schedulePreview: "every day at 04:00",
          nextRun: "2026-06-16T02:00:00.000Z",
        },
        {
          id: "restore_drill",
          enabled: false,
          frequency: "monthly",
          time: "05:00",
          schedulePreview: "on the 1st of each month at 05:00",
          nextRun: "2026-07-01T03:00:00.000Z",
        },
        {
          id: "container_update_check",
          enabled: true,
          frequency: "daily",
          time: "06:00",
          schedulePreview: "every day at 06:00",
          nextRun: "2026-06-16T04:00:00.000Z",
        },
      ],
      conflicts: [],
    };
    mockGet.mockResolvedValue({ data: schedulesPayload, response: { ok: true } });
    mockPut.mockResolvedValue({
      data: {
        ...schedulesPayload,
        otherJobs: schedulesPayload.otherJobs.map((job) =>
          job.id === "smart_self_test" ? { ...job, time: "04:15" } : job,
        ),
      },
      response: { ok: true },
    });

    renderWithToast(<SchedulesSettingsPage />);

    const timeInput = await screen.findByDisplayValue("03:00");
    fireEvent.change(timeInput, { target: { value: "04:15" } });
    expect(mockPut).not.toHaveBeenCalled();

    fireEvent.blur(timeInput);
    await waitFor(() => {
      expect(mockPut).toHaveBeenCalledWith("/settings/schedules/jobs/{jobId}", {
        params: { path: { jobId: "smart_self_test" } },
        body: { time: "04:15" },
      });
    });
  });

  it("routes Update, Rollback and Reboot through confirm", async () => {
    const updateStatus = {
      currentVersion: "0.1.0",
      availableVersion: "0.2.0",
      channel: "stable",
      checkEnabled: true,
      previousVersion: "0.0.1",
      rebootRequired: false,
      pendingDebianUpdates: [{ name: "openssl", installedVersion: "3.0.13", candidateVersion: "3.0.14" }],
      dependencies: [{ name: "mergerfs", installedVersion: "2.40.2", testedFloor: "2.40.2", inRange: true }],
    };
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/updates") {
        return Promise.resolve({ data: updateStatus, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPut.mockResolvedValue({ data: updateStatus, response: { ok: true } });
    mockPost.mockResolvedValue({ data: updateStatus, response: { ok: true } });

    renderWithToast(<UpdatesSettingsPage />);

    expect(await screen.findByText("0.1.0")).toBeInTheDocument();
    expect(screen.getByText("openssl 3.0.13 → 3.0.14")).toBeInTheDocument();

    fireEvent.click(await screen.findByRole("button", { name: "Update Hoserva" }));
    let dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Update Hoserva?")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Update Hoserva" }));
    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledWith("/settings/updates/apply", { body: { confirm: true } });
    });

    fireEvent.click(screen.getByRole("button", { name: "Rollback" }));
    dialog = await screen.findByRole("dialog", { name: "Rollback Hoserva?" });
    expect(within(dialog).getByText("Rollback Hoserva?")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Rollback" }));
    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledWith("/settings/updates/rollback", { body: { confirm: true } });
    });

    fireEvent.click(screen.getByRole("button", { name: "Reboot" }));
    dialog = await screen.findByRole("dialog", { name: "Reboot the server?" });
    expect(within(dialog).getByText("Reboot the server?")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Reboot" }));
    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledWith("/settings/updates/reboot", { body: { confirm: true } });
    });
  });
});
