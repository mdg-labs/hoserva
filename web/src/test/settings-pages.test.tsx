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
const mockPatch = vi.fn();
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
    PATCH: (...args: unknown[]) => mockPatch(...args),
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
    mockPatch.mockReset();
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
    mockPatch.mockReset();
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

  it("edits a destination's enabled flag and retention in place through updateBackupDestination", async () => {
    mockBackupApi();
    mockPatch.mockReturnValue(apiOk(backupDestination({ enabled: false, retention: { daily: 3, weekly: 0, monthly: 6 } })));

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Edit Boot device" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Edit Boot device")).toBeInTheDocument();
    const save = within(dialog).getByRole("button", { name: "Save changes" });
    expect(save).toBeDisabled();

    fireEvent.click(within(dialog).getByRole("switch", { name: "Enabled" }));
    fireEvent.change(within(dialog).getByLabelText("Daily"), { target: { value: "3" } });
    fireEvent.change(within(dialog).getByLabelText("Weekly"), { target: { value: "0" } });
    fireEvent.click(save);

    await waitFor(() =>
      expect(mockPatch).toHaveBeenCalledWith("/backup/destinations/{destinationId}", {
        params: { path: { destinationId: "boot" } },
        body: { enabled: false, retention: { daily: 3, weekly: 0, monthly: 6 } },
      }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(mockToast).toHaveBeenCalledWith(expect.objectContaining({ type: "success", title: "Destination updated" }));
    expect(mockGet.mock.calls.filter(([path]) => path === "/backup/destinations").length).toBeGreaterThan(1);
  });

  it("keeps the edit form open with the server's message when the update is refused", async () => {
    mockBackupApi();
    mockPatch.mockReturnValue(apiFail("backup_destination_invalid", "retention must keep at least one archive"));

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Edit Boot device" }));
    const dialog = await screen.findByRole("dialog");
    for (const label of ["Daily", "Weekly", "Monthly"]) {
      fireEvent.change(within(dialog).getByLabelText(label), { target: { value: "0" } });
    }
    fireEvent.click(within(dialog).getByRole("button", { name: "Save changes" }));

    expect(await within(dialog).findByText("retention must keep at least one archive")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(mockToast).not.toHaveBeenCalledWith(expect.objectContaining({ type: "success" }));
  });

  it("shows a rejected update request as an error and keeps the form", async () => {
    mockBackupApi();
    mockPatch.mockRejectedValue(new Error("network down"));

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Edit Boot device" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("switch", { name: "Enabled" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Save changes" }));

    expect(await within(dialog).findByText("network down")).toBeInTheDocument();
    expect(mockToast).not.toHaveBeenCalledWith(expect.objectContaining({ type: "success" }));
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

const EMPTY_CHANGES: unknown[] = [];

function importPreview(overrides: Record<string, unknown> = {}) {
  return {
    archive: {
      timestamp: "2026-09-01T03:00:00Z",
      host: "old-nas",
      hoservaVersion: "0.4.0",
      schemaVersion: "12",
    },
    liveSchemaVersion: "12",
    blockers: [],
    groups: [
      {
        category: "shares",
        added: [{ kind: "share", name: "photos" }],
        changed: [{ kind: "share", name: "media" }],
        removed: EMPTY_CHANGES,
      },
      { category: "accounts", added: EMPTY_CHANGES, changed: EMPTY_CHANGES, removed: [{ kind: "user", name: "guest" }] },
      { category: "system", added: EMPTY_CHANGES, changed: EMPTY_CHANGES, removed: EMPTY_CHANGES },
    ],
    secrets: { status: "opened", stacks: [] },
    notes: [
      { code: "sessions_replaced", message: "Active sign-in sessions are replaced by the archive's." },
      { code: "array_state_kept", message: "The array's current state is kept." },
    ],
    ...overrides,
  };
}

function bareMetalPreview(overrides: Record<string, unknown> = {}) {
  return {
    schemaUpgrade: true,
    disks: [
      {
        name: "Data disk 1 (/mnt/disk1)",
        role: "data",
        roleIndex: 1,
        mountpoint: "/mnt/disk1",
        fsUuid: "uuid-1",
        weakIdentity: false,
        state: "matched",
        device: "/dev/sdb",
      },
      {
        name: "Data disk 2 (/mnt/disk2)",
        role: "data",
        roleIndex: 2,
        mountpoint: "/mnt/disk2",
        fsUuid: "uuid-2",
        weakIdentity: false,
        state: "absent",
      },
    ],
    diskMapping: { disks: [{ role: "data", roleIndex: 1, device: "/dev/sdb" }] },
    ...overrides,
  };
}

function importReport(overrides: Record<string, unknown> = {}) {
  return {
    restored: [
      { category: "shares", added: 1, changed: 1, removed: 0 },
      { category: "accounts", added: 0, changed: 0, removed: 1 },
    ],
    notRestored: [
      {
        kind: "stack_env",
        name: "jellyfin",
        reason: "no_passphrase",
        message: "no passphrase was available to open the .env files",
      },
    ],
    secrets: "no_passphrase",
    preImportArchive: "hoserva-config-pre-import-2026-09-29.tar.zst",
    preImportSecrets: "configured",
    ...overrides,
  };
}

function multipartFields(call: unknown[]): FormData {
  const options = call[1] as { bodySerializer: () => FormData };
  return options.bodySerializer();
}

function mockRestoreApi(responses: { preview?: Promise<unknown>; apply?: Promise<unknown> } = {}): void {
  mockBackupApi();
  mockPost.mockImplementation((path: string) => {
    if (path === "/config/import/preview") {
      return responses.preview ?? apiOk(importPreview());
    }
    if (path === "/config/import") {
      return responses.apply ?? apiOk(importReport());
    }
    return Promise.resolve({ data: null, response: { ok: false } });
  });
}

async function previewArchive(passphrase?: string): Promise<void> {
  const archive = new File(["x"], "hoserva-config-old.tar.zst");
  fireEvent.change(await screen.findByLabelText("Config backup archive"), { target: { files: [archive] } });
  if (passphrase !== undefined) {
    fireEvent.change(screen.getByLabelText("Backup passphrase (optional)"), { target: { value: passphrase } });
  }
  fireEvent.click(screen.getByRole("button", { name: "Preview restore" }));
}

async function confirmRestore(): Promise<HTMLElement> {
  fireEvent.click(await screen.findByRole("button", { name: "Restore this configuration" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "restore" } });
  return dialog;
}

describe("Config restore on the backup page", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
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

  it("does not call the API until the preview button is used", async () => {
    mockRestoreApi();

    renderWithToast(<BackupSettingsPage />);
    expect(await screen.findByRole("button", { name: "Preview restore" })).toBeDisabled();
    fireEvent.change(screen.getByLabelText("Config backup archive"), {
      target: { files: [new File(["x"], "a.tar.zst")] },
    });

    expect(screen.getByRole("button", { name: "Preview restore" })).toBeEnabled();
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("sends the archive and passphrase as a multipart upload and shows who made it and what would change", async () => {
    mockRestoreApi();

    renderWithToast(<BackupSettingsPage />);
    await previewArchive("correct horse");

    expect(await screen.findByText("old-nas")).toBeInTheDocument();
    expect(screen.getByText("0.4.0")).toBeInTheDocument();
    const call = mockPost.mock.calls.find((entry) => entry[0] === "/config/import/preview") as unknown[];
    const form = multipartFields(call);
    expect((form.get("archive") as File).name).toBe("hoserva-config-old.tar.zst");
    expect(form.get("passphrase")).toBe("correct horse");

    expect(screen.getByText("Shares")).toBeInTheDocument();
    expect(screen.getByText("Share: photos")).toBeInTheDocument();
    expect(screen.getByText("Share: media")).toBeInTheDocument();
    expect(screen.getByText("User: guest")).toBeInTheDocument();
    expect(screen.getByText("Added from the archive")).toBeInTheDocument();
    expect(screen.getByText("Removed, because it is not in the archive")).toBeInTheDocument();
    expect(screen.queryByText("System settings")).not.toBeInTheDocument();
    expect(screen.getByText("You will be signed out")).toBeInTheDocument();
    expect(screen.getByText("Active sign-in sessions are replaced by the archive's.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Restore this configuration" })).toBeEnabled();
  });

  it("omits the passphrase part when none was entered", async () => {
    mockRestoreApi();

    renderWithToast(<BackupSettingsPage />);
    await previewArchive();

    await screen.findByText("old-nas");
    const call = mockPost.mock.calls.find((entry) => entry[0] === "/config/import/preview") as unknown[];
    expect(multipartFields(call).has("passphrase")).toBe(false);
  });

  it("says when an archive differs from the current configuration in nothing", async () => {
    mockRestoreApi({ preview: apiOk(importPreview({ groups: [] })) });

    renderWithToast(<BackupSettingsPage />);
    await previewArchive();

    expect(await screen.findByText("Nothing in this archive differs from the current configuration.")).toBeInTheDocument();
  });

  it("says the changes cannot be listed when the schema versions differ", async () => {
    mockRestoreApi({
      preview: apiOk(
        importPreview({ groups: [], archive: { ...importPreview().archive, schemaVersion: "9" } }),
      ),
    });

    renderWithToast(<BackupSettingsPage />);
    await previewArchive();

    expect(await screen.findByText("The changes cannot be listed")).toBeInTheDocument();
    expect(screen.queryByText("Nothing in this archive differs from the current configuration.")).not.toBeInTheDocument();
  });

  it("lists the changes of a fresh box's upgraded archive although the schema versions differ", async () => {
    mockRestoreApi({
      preview: apiOk(
        importPreview({ archive: { ...importPreview().archive, schemaVersion: "9" }, bareMetal: bareMetalPreview() }),
      ),
    });

    renderWithToast(<BackupSettingsPage />);
    await previewArchive();

    expect(await screen.findByText("Share: photos")).toBeInTheDocument();
    expect(screen.queryByText("The changes cannot be listed")).not.toBeInTheDocument();
  });

  it("shows each blocker as an error with its message and keeps the restore disabled", async () => {
    mockRestoreApi({
      preview: apiOk(
        importPreview({
          blockers: [
            { code: "archive_other_installation", message: "the archive was made by another installation" },
            { code: "archive_array_mismatch", message: "disk 3 is not in the live array" },
          ],
        }),
      ),
    });

    renderWithToast(<BackupSettingsPage />);
    await previewArchive();

    expect(await screen.findByText("This archive was made by a different installation")).toBeInTheDocument();
    expect(screen.getByText("the archive was made by another installation")).toBeInTheDocument();
    expect(screen.getByText("This archive describes a different array")).toBeInTheDocument();
    expect(screen.getByText("disk 3 is not in the live array")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Restore this configuration" })).toBeDisabled();
  });

  it.each([
    ["none", "No secrets section", []],
    ["opened", "Secrets open", []],
    ["no_passphrase", "No passphrase available", ["jellyfin", "nextcloud"]],
    ["passphrase_incorrect", "Passphrase does not open it", ["jellyfin"]],
  ])("shows secrets status %s and the stacks whose .env files would not be restored", async (status, label, stacks) => {
    mockRestoreApi({ preview: apiOk(importPreview({ secrets: { status, stacks } })) });

    renderWithToast(<BackupSettingsPage />);
    await previewArchive();

    expect(await screen.findByText(label)).toBeInTheDocument();
    for (const stack of stacks) {
      expect(screen.getByText(stack)).toBeInTheDocument();
    }
    if (stacks.length === 0) {
      expect(screen.queryByText("The .env files of these apps would not be restored:")).not.toBeInTheDocument();
    }
  });

  it("restores in place through a typed confirmation, without a disk mapping, and shows the report", async () => {
    mockRestoreApi();

    renderWithToast(<BackupSettingsPage />);
    await previewArchive("correct horse");
    fireEvent.click(await screen.findByRole("button", { name: "Restore this configuration" }));
    const dialog = await screen.findByRole("dialog");
    const restoreButton = within(dialog).getByRole("button", { name: "Restore" });
    expect(restoreButton).toBeDisabled();
    expect(within(dialog).getByText("You will be signed out when the restore finishes.")).toBeInTheDocument();
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "restore" } });
    expect(restoreButton).toBeEnabled();
    fireEvent.click(restoreButton);

    expect(await screen.findByText("The restore is finished and you were signed out")).toBeInTheDocument();
    const call = mockPost.mock.calls.find((entry) => entry[0] === "/config/import") as unknown[];
    const form = multipartFields(call);
    expect(form.get("confirm")).toBe("true");
    expect(form.get("passphrase")).toBe("correct horse");
    expect(form.has("diskMapping")).toBe(false);
    expect((form.get("archive") as File).name).toBe("hoserva-config-old.tar.zst");

    expect(screen.getByRole("link", { name: "Go to sign in" })).toHaveAttribute("href", "/login");
    expect(screen.getByText("What was restored")).toBeInTheDocument();
    expect(screen.getByText("jellyfin", { exact: false })).toBeInTheDocument();
    expect(screen.getByText("No passphrase")).toBeInTheDocument();
    expect(screen.getByText("no passphrase was available to open the .env files")).toBeInTheDocument();
    expect(
      screen.getByText("The configuration as it was before is saved as hoserva-config-pre-import-2026-09-29.tar.zst."),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Preview restore" })).not.toBeInTheDocument();
  });

  it("says when everything was restored and no safety archive was written", async () => {
    mockRestoreApi({ apply: apiOk(importReport({ notRestored: [], preImportArchive: "", preImportSecrets: "none" })) });

    renderWithToast(<BackupSettingsPage />);
    await previewArchive();
    await confirmRestore();
    fireEvent.click(screen.getByRole("button", { name: "Restore" }));

    expect(await screen.findByText("Everything in the archive was restored.")).toBeInTheDocument();
    expect(screen.getByText("No backup destination was written to before the restore.")).toBeInTheDocument();
  });

  it("shows the schema decision and the disk mapping of a fresh box and requires confirming it", async () => {
    mockRestoreApi({ preview: apiOk(importPreview({ bareMetal: bareMetalPreview() })) });

    renderWithToast(<BackupSettingsPage />);
    await previewArchive();

    expect(await screen.findByText("Data disk 1 (/mnt/disk1)")).toBeInTheDocument();
    expect(
      screen.getByText("The archive is from an older version. Its data is upgraded to this version while it is restored."),
    ).toBeInTheDocument();
    expect(screen.getByText("/dev/sdb")).toBeInTheDocument();
    expect(screen.getByText("Matched")).toBeInTheDocument();
    expect(screen.getByText("Not attached")).toBeInTheDocument();
    expect(screen.getByText("1 disk is not matched")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Restore this configuration" })).toBeDisabled();

    fireEvent.click(screen.getByRole("switch", { name: "This disk mapping is right" }));
    expect(screen.getByRole("button", { name: "Restore this configuration" })).toBeEnabled();
    fireEvent.click(screen.getByRole("switch", { name: "This disk mapping is right" }));
    expect(screen.getByRole("button", { name: "Restore this configuration" })).toBeDisabled();
  });

  it("sends the mapping it showed as diskMapping when restoring a fresh box", async () => {
    mockRestoreApi({ preview: apiOk(importPreview({ bareMetal: bareMetalPreview() })) });

    renderWithToast(<BackupSettingsPage />);
    await previewArchive();
    fireEvent.click(await screen.findByRole("switch", { name: "This disk mapping is right" }));
    await confirmRestore();
    fireEvent.click(screen.getByRole("button", { name: "Restore" }));

    await screen.findByText("The restore is finished and you were signed out");
    const call = mockPost.mock.calls.find((entry) => entry[0] === "/config/import") as unknown[];
    expect(JSON.parse(multipartFields(call).get("diskMapping") as string)).toEqual({
      disks: [{ role: "data", roleIndex: 1, device: "/dev/sdb" }],
    });
  });

  it("names the replaced and ambiguous disk states and says when the archive records no disks", async () => {
    const disk = (state: string, index: number) => ({
      ...bareMetalPreview().disks[0],
      name: `Disk ${index}`,
      roleIndex: index,
      state,
    });
    mockRestoreApi({
      preview: apiOk(
        importPreview({
          bareMetal: bareMetalPreview({ schemaUpgrade: false, disks: [disk("replaced", 1), disk("ambiguous", 2)] }),
        }),
      ),
    });

    renderWithToast(<BackupSettingsPage />);
    await previewArchive();

    expect(await screen.findByText("Replaced")).toBeInTheDocument();
    expect(screen.getByText("Ambiguous")).toBeInTheDocument();
    expect(screen.getByText("2 disks are not matched")).toBeInTheDocument();
    expect(screen.getByText("The archive's data format matches this version and is restored as it is.")).toBeInTheDocument();
  });

  it("discards the preview when the archive or the passphrase changes", async () => {
    mockRestoreApi();

    renderWithToast(<BackupSettingsPage />);
    await previewArchive();
    await screen.findByText("old-nas");

    fireEvent.change(screen.getByLabelText("Backup passphrase (optional)"), { target: { value: "x" } });

    expect(screen.queryByText("old-nas")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restore this configuration" })).not.toBeInTheDocument();
  });
});

const JELLYFIN = {
  name: "jellyfin",
  image: "jellyfin/jellyfin",
  running: true,
  stop: true,
  included: true,
  databaseImage: false,
};

const POSTGRES = {
  name: "postgres",
  image: "postgres",
  running: true,
  stop: true,
  included: true,
  databaseImage: true,
};

const POSTGRES_WARNING = "A database copied while it runs can give a backup that does not restore.";

function appdataJob(type: string, status: string, overrides: Record<string, unknown> = {}) {
  return {
    id: `${type}-job`,
    type,
    class: "service",
    status,
    progress: status === "running" ? 40 : null,
    resumable: false,
    cancellable: false,
    createdAt: "2026-09-29T10:00:00Z",
    ...overrides,
  };
}

function appdataArchive(overrides: Record<string, unknown> = {}) {
  return {
    name: "jellyfin-2026-09-28.tar.zst",
    container: "jellyfin",
    destinationId: "boot",
    destinationName: "Boot device",
    createdAt: "2026-09-28T02:00:00Z",
    size: 2048,
    encrypted: false,
    ...overrides,
  };
}

function restorePreview() {
  const empty = { files: 0, bytes: 0, sample: [] };
  return {
    container: "jellyfin",
    archive: "jellyfin-2026-09-28.tar.zst",
    destinationId: "boot",
    createdAt: "2026-09-28T02:00:00Z",
    directories: [
      {
        directory: "jellyfin/config",
        replaced: { files: 3, bytes: 3072, sample: ["a.db", "b.db"] },
        added: { files: 1, bytes: 10, sample: ["new.xml"] },
        removed: { files: 2, bytes: 4096, sample: ["old.log", "old2.log"] },
      },
      { directory: "jellyfin/cache", replaced: empty, added: empty, removed: empty },
    ],
  };
}

type AppdataResponses = {
  policy?: () => Promise<unknown>;
  archives?: () => Promise<unknown>;
  job?: (jobId: string) => Promise<unknown>;
  preview?: () => Promise<unknown>;
};

function mockAppdataApi(responses: AppdataResponses = {}): void {
  mockBackupApi();
  const base = mockGet.getMockImplementation() as (path: string) => unknown;
  mockGet.mockImplementation((path: string, options?: { params?: { path?: { jobId?: string } } }) => {
    switch (path) {
      case "/appdata/backup":
        return (responses.policy ?? (() => apiOk({ containers: [JELLYFIN, POSTGRES] })))();
      case "/appdata/backup/archives":
        return (responses.archives ?? (() => apiOk({ archives: [appdataArchive()], unavailable: [] })))();
      case "/jobs/{jobId}": {
        const jobId = options?.params?.path?.jobId ?? "";
        return (responses.job ?? ((id: string) => apiOk(appdataJob(id.replace(/-job$/, ""), "succeeded"))))(jobId);
      }
      case "/appdata/backup/restore/preview/{jobId}":
        return (responses.preview ?? (() => apiOk(restorePreview())))();
      default:
        return base(path);
    }
  });
  mockPost.mockImplementation((path: string) => {
    switch (path) {
      case "/appdata/backup":
        return apiOk(appdataJob("appdata_backup", "queued"));
      case "/appdata/backup/restore/preview":
        return apiOk(appdataJob("appdata_restore_preview", "queued"));
      case "/appdata/backup/restore":
        return apiOk(appdataJob("appdata_restore", "queued"));
      default:
        return Promise.resolve({ data: null, response: { ok: false } });
    }
  });
}

function getCalls(path: string): number {
  return mockGet.mock.calls.filter((call) => call[0] === path).length;
}

describe("Appdata backup on the backup page", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockPut.mockReset();
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

  it("lists each container with its stop policy and flags a database image that is not stopped with the API's warning", async () => {
    mockAppdataApi({
      policy: () =>
        apiOk({ containers: [JELLYFIN, { ...POSTGRES, stop: false, warning: POSTGRES_WARNING }] }),
    });

    renderWithToast(<BackupSettingsPage />);

    expect(await screen.findByText("jellyfin/jellyfin")).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Stop jellyfin while its appdata is copied" })).toBeChecked();
    expect(screen.getByRole("switch", { name: "Stop postgres while its appdata is copied" })).not.toBeChecked();
    expect(screen.getAllByText(POSTGRES_WARNING)).toHaveLength(1);
    expect(screen.getByText("Database image")).toBeInTheDocument();
  });

  it("saves a container's stop policy and shows the warning once a database image is not stopped", async () => {
    let postgres: Record<string, unknown> = POSTGRES;
    mockAppdataApi({ policy: () => apiOk({ containers: [JELLYFIN, postgres] }) });
    mockPut.mockImplementation(() => {
      postgres = { ...POSTGRES, stop: false, warning: POSTGRES_WARNING };
      return apiOk(postgres);
    });

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("switch", { name: "Stop postgres while its appdata is copied" }));

    expect(await screen.findByText(POSTGRES_WARNING)).toBeInTheDocument();
    expect(mockPut).toHaveBeenCalledWith("/appdata/backup/containers/{name}", {
      params: { path: { name: "postgres" } },
      body: { stop: false, included: true },
    });
    expect(screen.getByRole("switch", { name: "Stop postgres while its appdata is copied" })).not.toBeChecked();
  });

  it("saves whether a container is included without changing its stop policy", async () => {
    mockAppdataApi();
    mockPut.mockReturnValue(apiOk({ ...JELLYFIN, included: false }));

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("switch", { name: "Include jellyfin in the appdata backup" }));

    await waitFor(() =>
      expect(mockPut).toHaveBeenCalledWith("/appdata/backup/containers/{name}", {
        params: { path: { name: "jellyfin" } },
        body: { stop: true, included: false },
      }),
    );
  });

  it("starts a backup of every included container and follows the job to its end", async () => {
    mockAppdataApi();

    renderWithToast(<BackupSettingsPage />);
    await screen.findByText("jellyfin/jellyfin");
    const archiveLoads = getCalls("/appdata/backup/archives");
    fireEvent.click(screen.getByRole("button", { name: "Back up now" }));

    expect(await screen.findByText("Appdata backup finished")).toBeInTheDocument();
    expect(mockPost).toHaveBeenCalledWith("/appdata/backup", { body: {} });
    expect(mockGet).toHaveBeenCalledWith("/jobs/{jobId}", {
      params: { path: { jobId: "appdata_backup-job" } },
      signal: expect.anything(),
    });
    await waitFor(() => expect(getCalls("/appdata/backup/archives")).toBe(archiveLoads + 1));
  });

  it("starts a backup of one chosen container", async () => {
    mockAppdataApi();

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Back up postgres now" }));

    await screen.findByText("Appdata backup finished");
    expect(mockPost).toHaveBeenCalledWith("/appdata/backup", { body: { containers: ["postgres"] } });
  });

  it("shows the job's progress while it runs", async () => {
    let polls = 0;
    mockAppdataApi({
      job: () => {
        polls += 1;
        return apiOk(appdataJob("appdata_backup", polls === 1 ? "running" : "succeeded"));
      },
    });

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Back up now" }));

    expect(await screen.findByRole("progressbar")).toBeInTheDocument();
    expect(screen.queryByText("Appdata backup finished")).not.toBeInTheDocument();
    expect(await screen.findByText("Appdata backup finished", undefined, { timeout: 4000 })).toBeInTheDocument();
  });

  it("shows the job's own error when the backup job fails", async () => {
    mockAppdataApi({
      job: () =>
        apiOk(appdataJob("appdata_backup", "failed", { error: { code: "internal", message: "postgres would not stop" } })),
    });

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Back up now" }));

    expect(await screen.findByText("The appdata backup failed")).toBeInTheDocument();
    expect(screen.getByText("postgres would not stop")).toBeInTheDocument();
    expect(screen.queryByText("Appdata backup finished")).not.toBeInTheDocument();
  });

  it("lists archives newest first per container and destination", async () => {
    mockAppdataApi({
      archives: () =>
        apiOk({
          archives: [
            appdataArchive({ name: "jellyfin-old.tar.zst", createdAt: "2026-09-01T02:00:00Z" }),
            appdataArchive({ name: "jellyfin-new.tar.zst", createdAt: "2026-09-28T02:00:00Z", encrypted: true }),
            appdataArchive({ name: "jellyfin-offsite.tar.zst", destinationId: "offsite", destinationName: "Offsite copy" }),
            appdataArchive({ name: "postgres-1.tar.zst", container: "postgres" }),
            appdataArchive({ name: "jellyfin-snap.tar.zst", createdAt: "2026-09-15T02:00:00Z", reason: "pre-restore" }),
          ],
          unavailable: [],
        }),
    });

    renderWithToast(<BackupSettingsPage />);

    await screen.findByText("jellyfin-new.tar.zst");
    const order = screen
      .getAllByText(/^(jellyfin|postgres)-.*\.tar\.zst$/)
      .map((element) => element.textContent);
    expect(order).toEqual([
      "jellyfin-new.tar.zst",
      "jellyfin-snap.tar.zst",
      "jellyfin-old.tar.zst",
      "jellyfin-offsite.tar.zst",
      "postgres-1.tar.zst",
    ]);
    expect(screen.getByText("Snapshot before a restore")).toBeInTheDocument();
    const newestRow = screen.getByText("jellyfin-new.tar.zst").closest("tr") as HTMLElement;
    expect(within(newestRow).getByText("Encrypted")).toBeInTheDocument();
    const oldRow = screen.getByText("jellyfin-old.tar.zst").closest("tr") as HTMLElement;
    expect(within(oldRow).queryByText("Encrypted")).not.toBeInTheDocument();
  });

  it("names a destination that could not be listed instead of reading it as empty", async () => {
    mockAppdataApi({
      archives: () =>
        apiOk({ archives: [], unavailable: [{ destinationId: "offsite", message: "rclone: connection refused" }] }),
    });

    renderWithToast(<BackupSettingsPage />);

    expect(await screen.findByText("Offsite copy could not be listed")).toBeInTheDocument();
    expect(screen.getByText("rclone: connection refused")).toBeInTheDocument();
    expect(screen.getByText("None were found on the destinations that could be listed.")).toBeInTheDocument();
  });

  it("says there are no archives yet when every destination was listed and none holds one", async () => {
    mockAppdataApi({ archives: () => apiOk({ archives: [], unavailable: [] }) });

    renderWithToast(<BackupSettingsPage />);

    expect(await screen.findByText("No appdata archives yet")).toBeInTheDocument();
    expect(screen.queryByText(/could not be listed/)).not.toBeInTheDocument();
  });

  it("previews a restore, then restores after a typed confirmation and follows the restore job", async () => {
    mockAppdataApi();

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(
      await screen.findByRole("button", { name: "Restore jellyfin from jellyfin-2026-09-28.tar.zst" }),
    );

    const dialog = await screen.findByRole("dialog");
    expect(await within(dialog).findByText("jellyfin/config")).toBeInTheDocument();
    expect(mockPost).toHaveBeenCalledWith("/appdata/backup/restore/preview", {
      body: { container: "jellyfin", archive: "jellyfin-2026-09-28.tar.zst", destinationId: "boot" },
    });
    expect(mockGet).toHaveBeenCalledWith("/appdata/backup/restore/preview/{jobId}", {
      params: { path: { jobId: "appdata_restore_preview-job" } },
      signal: undefined,
    });
    expect(within(dialog).getByText("Replaced (3.00 KiB)")).toBeInTheDocument();
    expect(within(dialog).getByText("Added (10 B)")).toBeInTheDocument();
    expect(within(dialog).getByText("Removed (4.00 KiB)")).toBeInTheDocument();
    expect(within(dialog).getByText("a.db")).toBeInTheDocument();
    expect(within(dialog).getByText("old2.log")).toBeInTheDocument();
    expect(within(dialog).getByText("…and 1 more file")).toBeInTheDocument();
    expect(within(dialog).queryByText("jellyfin/cache")).not.toBeInTheDocument();

    const restoreButton = within(dialog).getByRole("button", { name: "Restore" });
    expect(restoreButton).toBeDisabled();
    expect(mockPost).not.toHaveBeenCalledWith("/appdata/backup/restore", expect.anything());
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "jellyfin" } });
    fireEvent.click(restoreButton);

    expect(await screen.findByText("Appdata restore finished")).toBeInTheDocument();
    expect(mockPost).toHaveBeenCalledWith("/appdata/backup/restore", {
      body: {
        container: "jellyfin",
        archive: "jellyfin-2026-09-28.tar.zst",
        destinationId: "boot",
        confirm: true,
      },
    });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("says nothing would change when the archive and the live files are identical", async () => {
    mockAppdataApi({
      preview: () => {
        const empty = { files: 0, bytes: 0, sample: [] };
        return apiOk({
          ...restorePreview(),
          directories: [{ directory: "jellyfin/config", replaced: empty, added: empty, removed: empty }],
        });
      },
    });

    renderWithToast(<BackupSettingsPage />);
    fireEvent.click(
      await screen.findByRole("button", { name: "Restore jellyfin from jellyfin-2026-09-28.tar.zst" }),
    );

    expect(
      await screen.findByText("The archive and the current files are identical, so nothing would change."),
    ).toBeInTheDocument();
  });
});
