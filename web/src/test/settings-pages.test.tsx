import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi, beforeEach } from "vitest";

import { ToastProvider } from "@/components/ui/toast";
import { BackupSettingsPage } from "@/routes/settings/backup";
import { GeneralSettingsPage } from "@/routes/settings/general";
import { NotificationsSettingsPage } from "@/routes/settings/notifications";
import { SchedulesSettingsPage } from "@/routes/settings/schedules";
import { UpdatesSettingsPage } from "@/routes/settings/updates";
import { NetworkSettingsPage } from "@/routes/settings/network";
import { MAINTENANCE_CHAIN_STEPS } from "@/lib/maintenance-chain";

const mockGet = vi.fn();
const mockPost = vi.fn();
const mockPut = vi.fn();
const mockDelete = vi.fn();
const mockToast = vi.fn();

vi.mock("@/components/patterns/feedback-toast", () => ({
  showFeedbackToast: (...args: unknown[]) => mockToast(...args),
}));

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
    PUT: (...args: unknown[]) => mockPut(...args),
    DELETE: (...args: unknown[]) => mockDelete(...args),
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
    mockDelete.mockReset();
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
      if (path === "/settings/ups") {
        return Promise.resolve({
          data: { configured: false },
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
    fireEvent.click(screen.getAllByRole("button", { name: "Save changes" })[0]);

    await waitFor(() => {
      expect(mockPut).toHaveBeenCalledWith("/settings/general", {
        body: {
          hostname: "hoserva",
          timezone: "Europe/Berlin",
        },
      });
    });
  });

  it("loads the UPS card empty state and saves a USB configuration", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/general") {
        return Promise.resolve({
          data: { hostname: "nas", timezone: "UTC", backupPassphraseSet: false },
          response: { ok: true },
        });
      }
      if (path === "/settings/ups") {
        return Promise.resolve({
          data: { configured: false },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPut.mockImplementation((path: string) => {
      if (path === "/settings/ups") {
        return Promise.resolve({
          data: {
            configured: true,
            connection: "usb",
            driver: "usbhid-ups",
            port: "auto",
            monitorPasswordSet: true,
            lowBatteryPercent: 20,
            runtimeSeconds: 300,
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderWithToast(<GeneralSettingsPage />);

    expect(
      await screen.findByText(/A UPS is optional/i),
    ).toBeInTheDocument();
    expect(screen.getByText("UPS")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Monitor password"), { target: { value: "s3cr3t" } });
    fireEvent.click(screen.getAllByRole("button", { name: "Save changes" })[1]);

    await waitFor(() => {
      expect(mockPut).toHaveBeenCalledWith("/settings/ups", {
        body: expect.objectContaining({
          connection: "usb",
          driver: "usbhid-ups",
          port: "auto",
          monitorPassword: "s3cr3t",
          lowBatteryPercent: 20,
          runtimeSeconds: 300,
        }),
      });
    });
  });

  it("omits a UPS password that was typed and then cleared, keeping the stored one", async () => {
    const saved = {
      configured: true,
      connection: "usb",
      driver: "usbhid-ups",
      port: "auto",
      monitorPasswordSet: true,
      lowBatteryPercent: 20,
      runtimeSeconds: 300,
    };
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/general") {
        return Promise.resolve({
          data: { hostname: "nas", timezone: "UTC", backupPassphraseSet: false },
          response: { ok: true },
        });
      }
      if (path === "/settings/ups") {
        return Promise.resolve({ data: saved, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPut.mockResolvedValue({ data: saved, response: { ok: true } });

    renderWithToast(<GeneralSettingsPage />);

    const password = await screen.findByLabelText("Monitor password");
    fireEvent.change(password, { target: { value: "typo" } });
    fireEvent.change(password, { target: { value: "" } });
    fireEvent.click(screen.getAllByRole("button", { name: "Save changes" })[1]);

    await waitFor(() => {
      expect(mockPut).toHaveBeenCalledWith("/settings/ups", expect.anything());
    });
    const [, request] = mockPut.mock.calls.find(([path]) => path === "/settings/ups") as [
      string,
      { body: Record<string, unknown> },
    ];
    expect(request.body).not.toHaveProperty("monitorPassword");
  });

  it("shows a UPS API error with the settings Banner pattern", async () => {
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/general") {
        return Promise.resolve({
          data: { hostname: "nas", timezone: "UTC", backupPassphraseSet: false },
          response: { ok: true },
        });
      }
      if (path === "/settings/ups") {
        return Promise.resolve({
          error: { message: "nut group is not present on this host" },
          response: { ok: false },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    renderWithToast(<GeneralSettingsPage />);

    expect(await screen.findByText("nut group is not present on this host")).toBeInTheDocument();
    expect(screen.queryByText(/A UPS is optional/i)).not.toBeInTheDocument();
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

  it("loads network interfaces and applies a static address", async () => {
    const networkPayload = {
      backend: "ifupdown",
      editable: true,
      interfaces: [
        {
          name: "enp1s0",
          mac: "02:00:00:00:00:01",
          method: "dhcp",
          address: "10.0.2.15",
          prefix: 24,
          gateway: "10.0.2.2",
          dns: ["1.1.1.1"],
          state: "up",
        },
        {
          name: "enp2s0",
          mac: "02:00:00:00:00:02",
          method: "dhcp",
          address: "10.0.3.15",
          prefix: 24,
          state: "up",
        },
      ],
      certificate: {
        kind: "self_signed",
        notAfter: "2036-09-20T00:00:00.000Z",
        daysRemaining: 3650,
      },
      letsEncrypt: {
        configured: false,
        enabled: false,
      },
      allowAllSources: false,
      listenPort: 8008,
    };
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/network") {
        return Promise.resolve({ data: networkPayload, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPut.mockResolvedValue({
      data: {
        ...networkPayload,
        pending: { interface: "enp1s0", expiresAt: "2026-09-20T12:01:00.000Z", remainingSeconds: 60 },
      },
      response: { ok: true },
    });

    renderWithToast(<NetworkSettingsPage />);

    expect((await screen.findAllByText("enp2s0")).length).toBeGreaterThan(0);
    const ifaceLabels = screen.getAllByText("enp2s0");
    fireEvent.click(ifaceLabels[ifaceLabels.length - 1]);
    fireEvent.click(screen.getByText("Static"));
    fireEvent.change(screen.getByLabelText("Address"), { target: { value: "192.0.2.1" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));

    await waitFor(() => {
      expect(mockPut).toHaveBeenCalledWith("/settings/network", {
        body: expect.objectContaining({
          interface: "enp2s0",
          method: "static",
          address: "192.0.2.1",
        }),
      });
    });
  });

  it("opens Let's Encrypt overlay and queues a DNS-01 issue", async () => {
    const networkPayload = {
      backend: "ifupdown",
      editable: true,
      interfaces: [
        {
          name: "enp1s0",
          method: "dhcp",
          address: "10.0.2.15",
          prefix: 24,
          state: "up",
        },
      ],
      certificate: {
        kind: "self_signed",
        notAfter: "2036-09-20T00:00:00.000Z",
        daysRemaining: 3650,
      },
      letsEncrypt: {
        configured: false,
        enabled: false,
      },
      allowAllSources: false,
      listenPort: 8008,
    };
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/network") {
        return Promise.resolve({ data: networkPayload, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPost.mockResolvedValue({
      data: {
        id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
        type: "acme_issue",
        class: "service",
        status: "queued",
        resumable: false,
        cancellable: true,
        createdAt: "2026-09-20T12:00:00.000Z",
      },
      response: { ok: true },
    });

    renderWithToast(<NetworkSettingsPage />);

    expect(await screen.findByText("Valid")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Set up Let's Encrypt" }));
    fireEvent.change(screen.getByPlaceholderText("nas.example.com"), { target: { value: "nas.example.com" } });
    fireEvent.change(screen.getByPlaceholderText("Zone.DNS Edit token"), { target: { value: "token" } });
    fireEvent.click(screen.getByRole("button", { name: "Issue certificate" }));

    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledWith("/settings/network/lets-encrypt", {
        body: expect.objectContaining({
          domain: "nas.example.com",
          provider: "cloudflare",
          cloudflareAPIToken: "token",
        }),
      });
    });
  });

  it("shows Let's Encrypt errors inside the overlay", async () => {
    const networkPayload = {
      backend: "ifupdown",
      editable: true,
      interfaces: [
        {
          name: "enp1s0",
          method: "dhcp",
          address: "10.0.2.15",
          prefix: 24,
          state: "up",
        },
      ],
      certificate: {
        kind: "self_signed",
        notAfter: "2036-09-20T00:00:00.000Z",
        daysRemaining: 3650,
      },
      letsEncrypt: {
        configured: false,
        enabled: false,
      },
      allowAllSources: false,
      listenPort: 8008,
    };
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/network") {
        return Promise.resolve({ data: networkPayload, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    mockPost.mockResolvedValue({
      error: { message: "acme: a DNS credential is required" },
      response: { ok: false },
    });

    renderWithToast(<NetworkSettingsPage />);

    expect(await screen.findByText("Valid")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Set up Let's Encrypt" }));
    fireEvent.click(screen.getByRole("button", { name: "Issue certificate" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Enter a domain name.")).toBeInTheDocument();

    fireEvent.change(within(dialog).getByPlaceholderText("nas.example.com"), { target: { value: "nas.example.com" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Issue certificate" }));

    expect(await within(dialog).findByText("acme: a DNS credential is required")).toBeInTheDocument();
  });

  it("does not close the Let's Encrypt overlay on Escape while the issue request is pending", async () => {
    const networkPayload = {
      backend: "ifupdown",
      editable: true,
      interfaces: [
        {
          name: "enp1s0",
          method: "dhcp",
          address: "10.0.2.15",
          prefix: 24,
          state: "up",
        },
      ],
      certificate: {
        kind: "self_signed",
        notAfter: "2036-09-20T00:00:00.000Z",
        daysRemaining: 3650,
      },
      letsEncrypt: {
        configured: false,
        enabled: false,
      },
      allowAllSources: false,
      listenPort: 8008,
    };
    mockGet.mockImplementation((path: string) => {
      if (path === "/settings/network") {
        return Promise.resolve({ data: networkPayload, response: { ok: true } });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });
    const pendingPost: { release: (() => void) | null } = { release: null };
    mockPost.mockImplementation(
      () =>
        new Promise((resolve) => {
          pendingPost.release = () => resolve({ error: { message: "acme: a DNS credential is required" }, response: { ok: false } });
        }),
    );

    renderWithToast(<NetworkSettingsPage />);

    expect(await screen.findByText("Valid")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Set up Let's Encrypt" }));
    fireEvent.click(screen.getByRole("button", { name: "Issue certificate" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Enter a domain name.")).toBeInTheDocument();

    fireEvent.change(within(dialog).getByPlaceholderText("nas.example.com"), { target: { value: "nas.example.com" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Issue certificate" }));
    await waitFor(() => expect(within(dialog).getByRole("button", { name: /Issue certificate/ })).toBeDisabled());
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeDisabled();

    fireEvent.keyDown(document, { key: "Escape", code: "Escape" });
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    pendingPost.release?.();
    expect(await within(dialog).findByText("acme: a DNS credential is required")).toBeInTheDocument();
  });
});

function apiOk(data: unknown) {
  return Promise.resolve({ data, response: { ok: true } });
}

function apiFail(code: string, message: string) {
  return Promise.resolve({ error: { code, message }, response: { ok: false } });
}

function backupDestination(overrides: Record<string, unknown> = {}) {
  return {
    id: "boot",
    name: "Boot device",
    type: "local",
    path: "/var/backups/hoserva",
    enabled: true,
    encrypt: false,
    retention: { daily: 7, weekly: 4, monthly: 6 },
    hasSecrets: false,
    stale: false,
    createdAt: "2026-09-01T00:00:00Z",
    lastSuccessfulBackupAt: "2026-09-28T02:00:00Z",
    ...overrides,
  };
}

const OFFSITE_DESTINATION = backupDestination({
  id: "offsite",
  name: "Offsite copy",
  type: "s3",
  path: "bucket/hoserva",
  encrypt: true,
  hasSecrets: true,
  stale: true,
  lastSuccessfulBackupAt: undefined,
});

function drillSchedule(overrides: Record<string, unknown> = {}) {
  return {
    chain: { startTime: "02:00", weeklyScrubDay: 0, schedulePreview: "every day at 02:00", nextRun: "2026-10-15T02:00:00Z", steps: [] },
    otherJobs: [
      {
        id: "restore_drill",
        enabled: true,
        frequency: "monthly",
        time: "04:00",
        schedulePreview: "monthly at 04:00",
        nextRun: "2026-10-15T12:00:00Z",
        ...overrides,
      },
    ],
    conflicts: [],
  };
}

type BackupApiResponses = {
  destinations?: Promise<unknown>;
  drill?: Promise<unknown>;
  schedules?: Promise<unknown>;
  general?: Promise<unknown>;
};

function mockBackupApi(responses: BackupApiResponses = {}): void {
  mockGet.mockImplementation((path: string) => {
    switch (path) {
      case "/backup/destinations":
        return responses.destinations ?? apiOk({ destinations: [backupDestination(), OFFSITE_DESTINATION] });
      case "/backup/drill":
        return responses.drill ?? apiOk({});
      case "/settings/schedules":
        return responses.schedules ?? apiOk(drillSchedule());
      case "/settings/general":
        return responses.general ?? apiOk({ backupPassphraseSet: true });
      default:
        return Promise.resolve({ data: null, response: { ok: false } });
    }
  });
}

async function openAddDestination(): Promise<HTMLElement> {
  await screen.findByText("Boot device");
  fireEvent.click(screen.getByRole("button", { name: "Add destination" }));
  return screen.findByRole("dialog");
}

describe("Backup settings page", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockPut.mockReset();
    mockDelete.mockReset();
    mockToast.mockReset();
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      addEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
      matches: false,
      media: query,
      onchange: null,
      removeEventListener: vi.fn(),
    })) as unknown as typeof window.matchMedia;
  });

  it("lists each destination with its type, retention, encryption, last backup and stale badge", async () => {
    mockBackupApi();

    renderWithToast(<BackupSettingsPage />);

    const bootRow = (await screen.findByText("Boot device")).closest("tr") as HTMLElement;
    expect(within(bootRow).getByText("Local folder")).toBeInTheDocument();
    expect(within(bootRow).getByText("Enabled")).toBeInTheDocument();
    expect(within(bootRow).getByText("7 daily, 4 weekly, 6 monthly")).toBeInTheDocument();
    expect(within(bootRow).getByText("Not encrypted")).toBeInTheDocument();
    expect(within(bootRow).getByText("Healthy")).toBeInTheDocument();
    expect(within(bootRow).queryByText("Stale")).not.toBeInTheDocument();

    const offsiteRow = screen.getByText("Offsite copy").closest("tr") as HTMLElement;
    expect(within(offsiteRow).getByText("S3-compatible storage")).toBeInTheDocument();
    expect(within(offsiteRow).getByText("Encrypted")).toBeInTheDocument();
    expect(within(offsiteRow).getByText("Never")).toBeInTheDocument();
    expect(within(offsiteRow).getByText("Stale")).toBeInTheDocument();

    expect(
      screen.getByText(
        "Backing up to an NFS share? Mount the share on this server, then add its mount path as a local folder.",
      ),
    ).toBeInTheDocument();
  });

  it("names the empty state when there are no destinations", async () => {
    mockBackupApi({ destinations: apiOk({ destinations: [] }) });

    renderWithToast(<BackupSettingsPage />);

    expect(await screen.findByText("No backup destinations yet")).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("offers the six destination types and no NFS remote", async () => {
    mockBackupApi();

    renderWithToast(<BackupSettingsPage />);
    const dialog = await openAddDestination();
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Type" }));

    const options = await screen.findAllByRole("option");
    expect(options.map((option) => option.textContent)).toEqual([
      "Local folder",
      "SMB share",
      "S3-compatible storage",
      "SFTP server",
      "WebDAV server",
      "rclone remote",
    ]);
    expect(screen.queryByRole("option", { name: /NFS/ })).not.toBeInTheDocument();
    expect(within(dialog).getByText(/For an NFS share, mount it first/)).toBeInTheDocument();
  });

  it("reports a passing connection test through a toast and keeps the button loading meanwhile", async () => {
    mockBackupApi();
    const pending: { release: (() => void) | null } = { release: null };
    mockPost.mockImplementation(
      () =>
        new Promise((resolve) => {
          pending.release = () => resolve({ data: { success: true }, response: { ok: true } });
        }),
    );

    renderWithToast(<BackupSettingsPage />);
    const button = await screen.findByRole("button", { name: "Test connection to Boot device" });
    fireEvent.click(button);

    await waitFor(() => expect(button).toBeDisabled());
    expect(mockPost).toHaveBeenCalledWith("/backup/destinations/{destinationId}/test", {
      params: { path: { destinationId: "boot" } },
    });

    pending.release?.();
    await waitFor(() => expect(button).not.toBeDisabled());
    expect(mockToast).toHaveBeenCalledWith(
      expect.objectContaining({ type: "success", title: "Connection test passed" }),
    );
  });

  it("shows the reason when a connection test comes back unsuccessful", async () => {
    mockBackupApi();
    mockPost.mockResolvedValue({ data: { success: false, error: "connection refused" }, response: { ok: true } });

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Test connection to Offsite copy" }));

    await waitFor(() =>
      expect(mockToast).toHaveBeenCalledWith({
        type: "error",
        title: "Connection test failed",
        description: "connection refused",
      }),
    );
  });

  it("reports a missing rclone with its install command", async () => {
    mockBackupApi();
    mockPost.mockReturnValue(apiFail("rclone_missing", "rclone is not installed; install it with: apt install rclone"));

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Test connection to Offsite copy" }));

    await waitFor(() =>
      expect(mockToast).toHaveBeenCalledWith({
        type: "error",
        title: "rclone is not installed",
        description: "rclone is not installed; install it with: apt install rclone",
      }),
    );
  });

  it("removes a destination after a confirmation that says its archives stay", async () => {
    mockBackupApi();
    mockDelete.mockResolvedValue({ response: { ok: true } });

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Remove Boot device" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Remove Boot device?")).toBeInTheDocument();
    expect(
      within(dialog).getByText("Archives already written to Boot device stay where they are. Hoserva does not delete them."),
    ).toBeInTheDocument();
    expect(mockDelete).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole("button", { name: "Remove destination" }));

    await waitFor(() =>
      expect(mockDelete).toHaveBeenCalledWith("/backup/destinations/{destinationId}", {
        params: { path: { destinationId: "boot" } },
      }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("keeps the removal confirmation open with the error when the removal fails", async () => {
    mockBackupApi();
    mockDelete.mockReturnValue(apiFail("backup_destination_not_found", "no backup destination with that id"));

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Remove Boot device" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove destination" }));

    expect(await within(dialog).findByText("no backup destination with that id")).toBeInTheDocument();
    expect(within(dialog).getByText(/stay where they are/)).toBeInTheDocument();
  });

  it("adds a local destination with the form's values", async () => {
    mockBackupApi();
    mockPost.mockReturnValue(apiOk(backupDestination({ id: "usb", name: "USB backup" })));

    renderWithToast(<BackupSettingsPage />);
    const dialog = await openAddDestination();
    const submit = within(dialog).getByRole("button", { name: "Add destination" });
    expect(submit).toBeDisabled();

    fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "USB backup" } });
    fireEvent.change(within(dialog).getByLabelText("Folder"), { target: { value: "/mnt/usb/backups" } });
    fireEvent.click(submit);

    await waitFor(() =>
      expect(mockPost).toHaveBeenCalledWith("/backup/destinations", {
        body: {
          name: "USB backup",
          type: "local",
          path: "/mnt/usb/backups",
          enabled: true,
          encrypt: false,
          retention: { daily: 7, weekly: 4, monthly: 6 },
        },
      }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(mockToast).toHaveBeenCalledWith(expect.objectContaining({ type: "success", title: "Destination added" }));
  });

  it("sends a remote destination's options and write-only credentials, and never an encrypt flag", async () => {
    mockBackupApi();
    mockPost.mockReturnValue(apiOk(backupDestination({ id: "s3", name: "Cloud", type: "s3" })));

    renderWithToast(<BackupSettingsPage />);
    const dialog = await openAddDestination();
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Type" }));
    const option = await screen.findByRole("option", { name: "S3-compatible storage" });
    fireEvent.pointerDown(option);
    fireEvent.click(option);

    fireEvent.change(await within(dialog).findByLabelText("Access key ID"), { target: { value: "AKIA123" } });
    fireEvent.change(within(dialog).getByLabelText("Secret access key"), { target: { value: "s3cr3t" } });
    fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Cloud" } });
    fireEvent.change(within(dialog).getByLabelText("Bucket and prefix"), { target: { value: "my-bucket/hoserva" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add destination" }));

    await waitFor(() =>
      expect(mockPost).toHaveBeenCalledWith("/backup/destinations", {
        body: {
          name: "Cloud",
          type: "s3",
          path: "my-bucket/hoserva",
          enabled: true,
          retention: { daily: 7, weekly: 4, monthly: 6 },
          options: { access_key_id: "AKIA123" },
          secrets: { secret_access_key: "s3cr3t" },
        },
      }),
    );
  });

  it("sends the user to set the passphrase when a remote destination needs one", async () => {
    mockBackupApi({ general: apiOk({ backupPassphraseSet: false }) });
    mockPost.mockReturnValue(
      apiFail("backup_passphrase_required", "a remote destination needs a backup passphrase"),
    );

    renderWithToast(<BackupSettingsPage />);
    const dialog = await openAddDestination();
    fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Local" } });
    fireEvent.change(within(dialog).getByLabelText("Folder"), { target: { value: "/mnt/x" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add destination" }));

    expect(await within(dialog).findByText("a remote destination needs a backup passphrase")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Set backup passphrase" }));

    const passphraseDialog = await screen.findByRole("dialog");
    expect(within(passphraseDialog).getByLabelText("Passphrase")).toBeInTheDocument();
    expect(within(passphraseDialog).queryByLabelText("Name")).not.toBeInTheDocument();
  });

  it("warns about the missing passphrase as soon as a remote type is picked", async () => {
    mockBackupApi({ general: apiOk({ backupPassphraseSet: false }) });

    renderWithToast(<BackupSettingsPage />);
    const dialog = await openAddDestination();
    expect(within(dialog).queryByText("Set a backup passphrase first")).not.toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole("combobox", { name: "Type" }));
    const option = await screen.findByRole("option", { name: "SFTP server" });
    fireEvent.pointerDown(option);
    fireEvent.click(option);

    expect(await within(dialog).findByText("Set a backup passphrase first")).toBeInTheDocument();
  });

  it("sets the backup passphrase write-only and never shows it", async () => {
    mockBackupApi({ general: apiOk({ backupPassphraseSet: false }) });
    mockPut.mockReturnValue(apiOk({ backupPassphraseSet: true }));

    renderWithToast(<BackupSettingsPage />);
    expect(await screen.findByText("No backup passphrase is set.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Set passphrase" }));

    const dialog = await screen.findByRole("dialog");
    const save = within(dialog).getByRole("button", { name: "Save passphrase" });
    fireEvent.change(within(dialog).getByLabelText("Passphrase"), { target: { value: "correct horse" } });
    fireEvent.change(within(dialog).getByLabelText("Repeat the passphrase"), { target: { value: "correct hors" } });
    expect(within(dialog).getByText("The two passphrases do not match.")).toBeInTheDocument();
    expect(save).toBeDisabled();

    fireEvent.change(within(dialog).getByLabelText("Repeat the passphrase"), { target: { value: "correct horse" } });
    fireEvent.click(save);

    await waitFor(() =>
      expect(mockPut).toHaveBeenCalledWith("/settings/general", { body: { backupPassphrase: "correct horse" } }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.queryByDisplayValue("correct horse")).not.toBeInTheDocument();
    expect(screen.queryByText("correct horse")).not.toBeInTheDocument();
  });

  it("shows that a passphrase is set and offers to change it", async () => {
    mockBackupApi({ general: apiOk({ backupPassphraseSet: true }) });

    renderWithToast(<BackupSettingsPage />);

    expect(await screen.findByText("A backup passphrase is set.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Change passphrase" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Change backup passphrase")).toBeInTheDocument();
    expect(within(dialog).getByText(/Keep the previous one for them/)).toBeInTheDocument();
  });

  it("keeps the passphrase overlay open with the error when saving fails", async () => {
    mockBackupApi({ general: apiOk({ backupPassphraseSet: false }) });
    mockPut.mockReturnValue(apiFail("bad_request", "passphrase rejected"));

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Set passphrase" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Passphrase"), { target: { value: "abc" } });
    fireEvent.change(within(dialog).getByLabelText("Repeat the passphrase"), { target: { value: "abc" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save passphrase" }));

    expect(await within(dialog).findByText("passphrase rejected")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("downloads the config archive", async () => {
    mockBackupApi();
    const archive = new Blob(["archive"]);
    mockPost.mockReturnValue(apiOk(archive));
    const createObjectURL = vi.fn(() => "blob:config");
    const revokeObjectURL = vi.fn();
    Object.defineProperty(URL, "createObjectURL", { configurable: true, writable: true, value: createObjectURL });
    Object.defineProperty(URL, "revokeObjectURL", { configurable: true, writable: true, value: revokeObjectURL });
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Download config backup" }));

    await waitFor(() => expect(click).toHaveBeenCalledTimes(1));
    expect(mockPost).toHaveBeenCalledWith("/config/export", { parseAs: "blob" });
    expect(createObjectURL).toHaveBeenCalledWith(archive);
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:config");
    expect(mockToast).toHaveBeenCalledWith(expect.objectContaining({ type: "success", title: "Config backup downloaded" }));
    click.mockRestore();
  });

  it("shows an error instead of saving a file when the config export fails", async () => {
    mockBackupApi();
    mockPost.mockReturnValue(apiFail("internal", "could not build the archive"));
    const createObjectURL = vi.fn(() => "blob:config");
    Object.defineProperty(URL, "createObjectURL", { configurable: true, writable: true, value: createObjectURL });

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Download config backup" }));

    expect(await screen.findByText("Could not download the config backup")).toBeInTheDocument();
    expect(screen.getByText("could not build the archive")).toBeInTheDocument();
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  it("names a restore drill that has never run, with the next scheduled run", async () => {
    mockBackupApi({ drill: apiOk({}) });

    renderWithToast(<BackupSettingsPage />);

    expect(await screen.findByText("Never run yet")).toBeInTheDocument();
    expect(await screen.findByText(/Oct 15, 2026/)).toBeInTheDocument();
  });

  it("says the restore drill is not scheduled when its schedule is off", async () => {
    mockBackupApi({ schedules: apiOk(drillSchedule({ enabled: false })) });

    renderWithToast(<BackupSettingsPage />);

    expect(await screen.findByText("Not scheduled. The restore drill schedule is off.")).toBeInTheDocument();
  });

  it("shows the last restore drill's result per destination with the reason each failed", async () => {
    mockBackupApi({
      drill: apiOk({
        lastRun: {
          ranAt: "2026-09-28T04:00:00Z",
          passed: false,
          destinations: [
            {
              destinationId: "boot",
              destinationName: "Boot device",
              passed: true,
              archive: "hoserva-config-x-2026-09-28.tar.zst",
            },
            {
              destinationId: "offsite",
              destinationName: "Offsite copy",
              passed: false,
              archive: null,
              error: "no archive written by this installation",
            },
          ],
        },
      }),
    });

    renderWithToast(<BackupSettingsPage />);

    expect(await screen.findByText("Archive tested: hoserva-config-x-2026-09-28.tar.zst")).toBeInTheDocument();
    expect(screen.getByText("No archive found")).toBeInTheDocument();
    expect(screen.getByText("no archive written by this installation")).toBeInTheDocument();
    expect(screen.getAllByText("Failed")).toHaveLength(2);
    expect(screen.queryByText("Never run yet")).not.toBeInTheDocument();
  });

  it("queues a restore drill and shows the job it queued", async () => {
    mockBackupApi();
    mockPost.mockReturnValue(
      apiOk({
        id: "3f9a1c52-0000-4000-8000-000000000001",
        type: "restore_drill",
        class: "service",
        status: "queued",
        resumable: false,
        cancellable: false,
        createdAt: "2026-09-29T10:00:00Z",
      }),
    );

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Run now" }));

    expect(await screen.findByText("Restore drill queued")).toBeInTheDocument();
    expect(mockPost).toHaveBeenCalledWith("/backup/drill");
    expect(screen.getByText(/Job status: Queued\./)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View job" })).toHaveAttribute(
      "href",
      "/jobs/3f9a1c52-0000-4000-8000-000000000001",
    );
  });

  it("shows the error when the restore drill cannot be started", async () => {
    mockBackupApi();
    mockPost.mockReturnValue(apiFail("not_configured", "no backup service is configured"));

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Run now" }));

    expect(await screen.findByText("Could not start the restore drill")).toBeInTheDocument();
    expect(screen.getByText("no backup service is configured")).toBeInTheDocument();
    expect(screen.queryByText("Restore drill queued")).not.toBeInTheDocument();
  });
});
