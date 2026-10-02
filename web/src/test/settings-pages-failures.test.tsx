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

describe("Config restore failures", () => {
  const PREVIEW = {
    archive: { timestamp: "2026-09-01T03:00:00Z", host: "old-nas", hoservaVersion: "0.4.0", schemaVersion: "12" },
    liveSchemaVersion: "12",
    blockers: [],
    groups: [],
    secrets: { status: "opened", stacks: [] },
    notes: [],
  };

  function ok(data: unknown) {
    return Promise.resolve({ data, response: { ok: true } });
  }

  function fail(code: string, message: string) {
    return Promise.resolve({ error: { code, message }, response: { ok: false } });
  }

  function restoreApi(responses: { preview?: () => unknown; apply?: () => unknown }): void {
    mockGet.mockImplementation((path: string) => {
      if (path === "/backup/destinations") {
        return ok({ destinations: [] });
      }
      if (path === "/backup/drill") {
        return ok({});
      }
      if (path === "/settings/general") {
        return ok({ backupPassphraseSet: true });
      }
      return notFound();
    });
    mockPost.mockImplementation((path: string) => {
      if (path === "/config/import/preview") {
        return (responses.preview ?? (() => ok(PREVIEW)))();
      }
      if (path === "/config/import") {
        return (responses.apply ?? (() => fail("internal", "unexpected")))();
      }
      return notFound();
    });
  }

  async function preview(): Promise<void> {
    render(
      <MemoryRouter>
        <BackupSettingsPage />
      </MemoryRouter>,
    );
    fireEvent.change(await screen.findByLabelText("Config backup archive"), {
      target: { files: [new File(["x"], "a.tar.zst")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Preview restore" }));
  }

  async function openApply(): Promise<HTMLElement> {
    await preview();
    fireEvent.click(await screen.findByRole("button", { name: "Restore this configuration" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "restore" } });
    return dialog;
  }

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

  it.each([
    ["invalid_archive", "This file is not a readable config backup"],
    ["archive_too_large", "This archive is too large to restore"],
    ["incompatible_archive", "This archive is from an incompatible version"],
    ["archive_newer_version", "This archive is from a newer Hoserva version"],
    ["archive_other_installation", "This archive was made by a different installation"],
    ["archive_array_mismatch", "This archive describes a different array"],
    ["backup_passphrase_incorrect", "That passphrase does not open this archive"],
    ["job_in_progress", "A job is running, so the restore cannot start"],
    ["not_configured", "This server has no backup service"],
  ])("names a preview refused as %s and shows no preview", async (code, title) => {
    restoreApi({ preview: () => fail(code, `server says ${code}`) });

    await preview();

    expect(await screen.findByText(title)).toBeInTheDocument();
    expect(screen.getByText(`server says ${code}`)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restore this configuration" })).not.toBeInTheDocument();
  });

  it("names a preview that failed for another reason with the server's message", async () => {
    restoreApi({ preview: () => fail("internal", "could not stage the archive") });

    await preview();

    expect(await screen.findByText("Could not preview the archive")).toBeInTheDocument();
    expect(screen.getByText("could not stage the archive")).toBeInTheDocument();
  });

  it("names a preview whose request was rejected, and one answered without a body", async () => {
    restoreApi({ preview: () => Promise.reject(new Error("network down")) });

    await preview();

    expect(await screen.findByText("Could not preview the archive")).toBeInTheDocument();
    expect(screen.getByText("network down")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restore this configuration" })).not.toBeInTheDocument();

    cleanup();
    restoreApi({ preview: () => Promise.resolve({ response: { ok: true } }) });

    await preview();

    expect(await screen.findByText("Could not preview the archive")).toBeInTheDocument();
    expect(screen.getByText("The server answered without a preview.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restore this configuration" })).not.toBeInTheDocument();
  });

  it.each([
    ["job_in_progress", "A job is running, so the restore cannot start"],
    ["backup_passphrase_incorrect", "That passphrase does not open this archive"],
    ["archive_other_installation", "This archive was made by a different installation"],
    ["host_files_not_saved", "There is nowhere to save this server's current Samba and NFS settings first"],
  ])("keeps the typed confirmation and names an apply refused as %s", async (code, title) => {
    restoreApi({ apply: () => fail(code, `apply says ${code}`) });

    const dialog = await openApply();
    fireEvent.click(within(dialog).getByRole("button", { name: "Restore" }));

    expect(await within(dialog).findByText(title)).toBeInTheDocument();
    expect(within(dialog).getByText(`apply says ${code}`)).toBeInTheDocument();
    expect(within(dialog).getByRole("textbox")).toHaveValue("restore");
    expect(within(dialog).getByRole("button", { name: "Restore" })).toBeEnabled();
    expect(screen.queryByText("The restore is finished and you were signed out")).not.toBeInTheDocument();
  });

  it("offers a new preview when the disk mapping went stale, and runs it", async () => {
    restoreApi({ apply: () => fail("disk_mapping_stale", "sdb is no longer attached") });

    const dialog = await openApply();
    fireEvent.click(within(dialog).getByRole("button", { name: "Restore" }));

    expect(await within(dialog).findByText("The attached disks changed since the preview")).toBeInTheDocument();
    const previewsBefore = mockPost.mock.calls.filter((call) => call[0] === "/config/import/preview").length;
    fireEvent.click(within(dialog).getByRole("button", { name: "Preview again" }));

    await waitFor(() =>
      expect(mockPost.mock.calls.filter((call) => call[0] === "/config/import/preview").length).toBe(
        previewsBefore + 1,
      ),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("names an apply whose request was rejected and does not show a report", async () => {
    restoreApi({ apply: () => Promise.reject(new Error("connection reset")) });

    const dialog = await openApply();
    fireEvent.click(within(dialog).getByRole("button", { name: "Restore" }));

    expect(await within(dialog).findByText("The restore failed")).toBeInTheDocument();
    expect(within(dialog).getByText("connection reset")).toBeInTheDocument();
    expect(screen.queryByText("What was restored")).not.toBeInTheDocument();
  });

  it("does not show a restore that succeeded without a report as a success", async () => {
    restoreApi({ apply: () => Promise.resolve({ response: { ok: true } }) });

    const dialog = await openApply();
    fireEvent.click(within(dialog).getByRole("button", { name: "Restore" }));

    expect(await within(dialog).findByText("The restore failed")).toBeInTheDocument();
    expect(
      within(dialog).getByText(
        "The server did not return a restore report. Check the destinations for the backup made before the restore.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText("What was restored")).not.toBeInTheDocument();
  });

  it("keeps the dialog open and the restore button busy while the restore runs", async () => {
    let finish: (value: unknown) => void = () => {};
    restoreApi({ apply: () => new Promise((resolve) => (finish = resolve)) });

    const dialog = await openApply();
    fireEvent.click(within(dialog).getByRole("button", { name: "Restore" }));
    await waitFor(() => expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeDisabled());
    fireEvent.keyDown(dialog, { key: "Escape" });

    expect(screen.getByRole("dialog")).toBeInTheDocument();
    finish({ data: { restored: [], notRestored: [], secrets: "none", preImportArchive: "", preImportSecrets: "none" }, response: { ok: true } });
    expect(await screen.findByText("The restore is finished and you were signed out")).toBeInTheDocument();
  });
});

describe("Appdata backup failures", () => {
  const JELLYFIN = {
    name: "jellyfin",
    image: "jellyfin/jellyfin",
    running: true,
    stop: true,
    included: true,
    databaseImage: false,
  };
  const ARCHIVE = {
    name: "jellyfin-2026-09-28.tar.zst",
    container: "jellyfin",
    destinationId: "boot",
    destinationName: "Boot device",
    createdAt: "2026-09-28T02:00:00Z",
    size: 2048,
    encrypted: false,
  };
  const PREVIEW = {
    container: "jellyfin",
    archive: ARCHIVE.name,
    destinationId: "boot",
    createdAt: "2026-09-28T02:00:00Z",
    directories: [
      {
        directory: "jellyfin/config",
        replaced: { files: 1, bytes: 10, sample: ["a.db"] },
        added: { files: 0, bytes: 0, sample: [] },
        removed: { files: 0, bytes: 0, sample: [] },
      },
    ],
  };

  function ok(data: unknown) {
    return Promise.resolve({ data, response: { ok: true } });
  }

  function fail(code: string, message: string, status = 409) {
    return Promise.resolve({ error: { code, message }, response: { ok: false, status } });
  }

  function job(type: string, status: string, overrides: Record<string, unknown> = {}) {
    return {
      id: `${type}-job`,
      type,
      class: "service",
      status,
      resumable: false,
      cancellable: false,
      createdAt: "2026-09-29T10:00:00Z",
      ...overrides,
    };
  }

  type Responses = {
    policy?: () => unknown;
    archives?: () => unknown;
    job?: () => unknown;
    preview?: () => unknown;
    post?: Record<string, () => unknown>;
    put?: () => unknown;
  };

  function appdataApi(responses: Responses): void {
    mockGet.mockImplementation((path: string) => {
      switch (path) {
        case "/backup/destinations":
          return ok({ destinations: [] });
        case "/backup/drill":
          return ok({});
        case "/settings/general":
          return ok({ backupPassphraseSet: true });
        case "/appdata/backup":
          return (responses.policy ?? (() => ok({ containers: [JELLYFIN] })))();
        case "/appdata/backup/archives":
          return (responses.archives ?? (() => ok({ archives: [ARCHIVE], unavailable: [] })))();
        case "/jobs/{jobId}":
          return (responses.job ?? (() => ok(job("appdata_restore_preview", "succeeded"))))();
        case "/appdata/backup/restore/preview/{jobId}":
          return (responses.preview ?? (() => ok(PREVIEW)))();
        default:
          return notFound();
      }
    });
    mockPost.mockImplementation((path: string) => {
      const handler = responses.post?.[path];
      if (handler) {
        return handler();
      }
      if (path === "/appdata/backup/restore/preview") {
        return ok(job("appdata_restore_preview", "queued"));
      }
      if (path === "/appdata/backup/restore") {
        return ok(job("appdata_restore", "queued"));
      }
      if (path === "/appdata/backup") {
        return ok(job("appdata_backup", "queued"));
      }
      return notFound();
    });
    mockPut.mockImplementation(() => (responses.put ?? (() => ok(JELLYFIN)))());
  }

  function renderPage(): void {
    render(
      <MemoryRouter>
        <BackupSettingsPage />
      </MemoryRouter>,
    );
  }

  async function openRestore(): Promise<HTMLElement> {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: `Restore jellyfin from ${ARCHIVE.name}` }));
    return screen.findByRole("dialog");
  }

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

  it("names a server without a Docker Engine client instead of an empty list", async () => {
    appdataApi({ policy: () => fail("not_configured", "no docker client", 501) });

    renderPage();

    expect(await screen.findByText("Apps are not available on this server")).toBeInTheDocument();
    expect(screen.queryByText("No apps with appdata")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Back up now" })).toBeDisabled();
  });

  it("names an unreachable Docker Engine with the server's message and offers to try again", async () => {
    let calls = 0;
    appdataApi({
      policy: () => {
        calls += 1;
        return calls === 1 ? fail("docker_unreachable", "dial unix /var/run/docker.sock: refused", 503) : ok({ containers: [JELLYFIN] });
      },
    });

    renderPage();

    expect(await screen.findByText("The Docker Engine is not reachable")).toBeInTheDocument();
    expect(screen.getByText("dial unix /var/run/docker.sock: refused")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("jellyfin/jellyfin")).toBeInTheDocument();
  });

  it("shows a banner instead of an empty table when the policy cannot be loaded", async () => {
    appdataApi({ policy: () => fail("internal", "policy store unavailable", 500) });

    renderPage();

    expect(await screen.findByText("Could not load the appdata backup settings")).toBeInTheDocument();
    expect(screen.getByText("policy store unavailable")).toBeInTheDocument();
    expect(screen.queryByText("No apps with appdata")).not.toBeInTheDocument();
  });

  it("names the state of no containers with appdata in scope", async () => {
    appdataApi({ policy: () => ok({ containers: [] }) });

    renderPage();

    expect(await screen.findByText("No apps with appdata")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Back up now" })).toBeDisabled();
  });

  it("names a policy change that could not be saved and keeps the old setting", async () => {
    appdataApi({ put: () => fail("container_not_found", "no such container", 404) });

    renderPage();
    const stop = await screen.findByRole("switch", { name: "Stop jellyfin while its appdata is copied" });
    fireEvent.click(stop);

    expect(await screen.findByText("That app has no appdata in the backup")).toBeInTheDocument();
    expect(screen.getByText("no such container")).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Stop jellyfin while its appdata is copied" })).toBeChecked();
  });

  it("builds the next toggle on the saved row when the refresh after a save fails", async () => {
    let reads = 0;
    appdataApi({
      policy: () => {
        reads += 1;
        return reads === 1 ? ok({ containers: [JELLYFIN] }) : fail("internal", "policy store unavailable", 500);
      },
      put: () => ok({ ...JELLYFIN, stop: false }),
    });

    renderPage();
    fireEvent.click(await screen.findByRole("switch", { name: "Stop jellyfin while its appdata is copied" }));
    expect(await screen.findByText("policy store unavailable")).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole("switch", { name: "Stop jellyfin while its appdata is copied" })).not.toBeChecked(),
    );
    await waitFor(() =>
      expect(screen.getByRole("switch", { name: "Include jellyfin in the appdata backup" })).toBeEnabled(),
    );
    fireEvent.click(screen.getByRole("switch", { name: "Include jellyfin in the appdata backup" }));

    await waitFor(() => expect(mockPut).toHaveBeenCalledTimes(2));
    expect(mockPut.mock.calls[0][1]).toMatchObject({ body: { stop: false, included: true } });
    expect(mockPut.mock.calls[1][1]).toMatchObject({ body: { stop: false, included: false } });
  });

  it("does not send a second change for a row while its first save is in flight", async () => {
    let finish: (value: unknown) => void = () => undefined;
    appdataApi({ put: () => new Promise((resolve) => (finish = resolve)) });

    renderPage();
    const stop = await screen.findByRole("switch", { name: "Stop jellyfin while its appdata is copied" });
    fireEvent.click(stop);
    fireEvent.click(stop);
    fireEvent.click(screen.getByRole("switch", { name: "Include jellyfin in the appdata backup" }));

    expect(mockPut).toHaveBeenCalledTimes(1);
    finish({ data: { ...JELLYFIN, stop: false }, response: { ok: true } });
    await waitFor(() =>
      expect(screen.getByRole("switch", { name: "Include jellyfin in the appdata backup" })).toBeEnabled(),
    );
  });

  it.each([
    ["array_stopped", "The array is stopped, so appdata cannot be backed up or restored", 409],
    ["container_not_found", "That app has no appdata in the backup", 404],
  ])("names a backup refused as %s and follows no job", async (code, title, status) => {
    appdataApi({ post: { "/appdata/backup": () => fail(code, `backup says ${code}`, status) } });

    renderPage();
    const backUp = await screen.findByRole("button", { name: "Back up now" });
    await waitFor(() => expect(backUp).toBeEnabled());
    fireEvent.click(backUp);

    expect(await screen.findByText(title)).toBeInTheDocument();
    expect(screen.getByText(`backup says ${code}`)).toBeInTheDocument();
    expect(screen.queryByRole("progressbar")).not.toBeInTheDocument();
    expect(mockGet).not.toHaveBeenCalledWith("/jobs/{jobId}", expect.anything());
  });

  it("names a backup whose request was rejected", async () => {
    appdataApi({ post: { "/appdata/backup": () => Promise.reject(new Error("network down")) } });

    renderPage();
    const backUp = await screen.findByRole("button", { name: "Back up now" });
    await waitFor(() => expect(backUp).toBeEnabled());
    fireEvent.click(backUp);

    expect(await screen.findByText("Could not start the appdata backup")).toBeInTheDocument();
    expect(screen.getByText("network down")).toBeInTheDocument();
  });

  it("names a job that cannot be followed instead of reading it as finished", async () => {
    appdataApi({ job: () => fail("internal", "job store unavailable", 500) });

    renderPage();
    const backUp = await screen.findByRole("button", { name: "Back up now" });
    await waitFor(() => expect(backUp).toBeEnabled());
    fireEvent.click(backUp);

    expect(await screen.findByText("Could not follow the job")).toBeInTheDocument();
    expect(screen.getByText("job store unavailable")).toBeInTheDocument();
    expect(screen.queryByText("Appdata backup finished")).not.toBeInTheDocument();
  });

  it("names a cancelled job with its status", async () => {
    appdataApi({ job: () => ok(job("appdata_backup", "cancelled")) });

    renderPage();
    const backUp = await screen.findByRole("button", { name: "Back up now" });
    await waitFor(() => expect(backUp).toBeEnabled());
    fireEvent.click(backUp);

    expect(await screen.findByText("The appdata backup failed")).toBeInTheDocument();
    expect(screen.getByText("Job status: Cancelled.")).toBeInTheDocument();
  });

  it("shows a banner when the archives cannot be loaded, and a named state when the server has no backup service", async () => {
    appdataApi({ archives: () => fail("internal", "listing crashed", 500) });

    renderPage();

    expect(await screen.findByText("Could not load the appdata archives")).toBeInTheDocument();
    expect(screen.getByText("listing crashed")).toBeInTheDocument();
    expect(screen.queryByText("No appdata archives yet")).not.toBeInTheDocument();

    cleanup();
    appdataApi({ archives: () => fail("not_configured", "no backup service", 501) });

    renderPage();

    expect(await screen.findByText("Backups are not available on this server")).toBeInTheDocument();
    expect(screen.queryByText("No appdata archives yet")).not.toBeInTheDocument();
  });

  it.each([
    ["archive_not_found", "That archive is no longer on the destination", 404],
    ["array_stopped", "The array is stopped, so appdata cannot be backed up or restored", 409],
    ["appdata_archive_invalid", "That is not an appdata archive of this server and app", 400],
  ])("names a restore preview that is refused as %s", async (code, title, status) => {
    appdataApi({ post: { "/appdata/backup/restore/preview": () => fail(code, `preview says ${code}`, status) } });

    const dialog = await openRestore();

    expect(await within(dialog).findByText(title)).toBeInTheDocument();
    expect(within(dialog).getByText(`preview says ${code}`)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Restore" })).toBeDisabled();
    expect(mockPost).not.toHaveBeenCalledWith("/appdata/backup/restore", expect.anything());
  });

  it("shows the job's own error when the preview job fails, for a corrupt or refused archive", async () => {
    appdataApi({
      job: () =>
        ok(
          job("appdata_restore_preview", "failed", {
            error: { code: "appdata_archive_invalid", message: "the archive holds a link to /etc" },
          }),
        ),
    });

    const dialog = await openRestore();

    expect(await within(dialog).findByText("Could not preview the restore")).toBeInTheDocument();
    expect(within(dialog).getByText("the archive holds a link to /etc")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Restore" })).toBeDisabled();
    expect(mockGet).not.toHaveBeenCalledWith("/appdata/backup/restore/preview/{jobId}", expect.anything());
  });

  it("names a preview that failed after the job ran, from the preview read", async () => {
    appdataApi({ preview: () => fail("appdata_preview_failed", "the job was cancelled", 409) });

    const dialog = await openRestore();

    expect(await within(dialog).findByText("The preview failed")).toBeInTheDocument();
    expect(within(dialog).getByText("the job was cancelled")).toBeInTheDocument();
  });

  it("offers to preview again when the result is gone, and shows the new preview", async () => {
    let reads = 0;
    appdataApi({
      preview: () => {
        reads += 1;
        return reads === 1 ? fail("appdata_preview_gone", "the result is no longer held", 404) : ok(PREVIEW);
      },
    });

    const dialog = await openRestore();

    expect(await within(dialog).findByText("The preview is no longer available")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Preview again" }));

    expect(await within(dialog).findByText("jellyfin/config")).toBeInTheDocument();
    expect(mockPost.mock.calls.filter((call) => call[0] === "/appdata/backup/restore/preview")).toHaveLength(2);
  });

  it("keeps the typed confirmation and names a restore refused while the array is stopped", async () => {
    appdataApi({ post: { "/appdata/backup/restore": () => fail("array_stopped", "array is stopped") } });

    const dialog = await openRestore();
    await within(dialog).findByText("jellyfin/config");
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "jellyfin" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Restore" }));

    expect(
      await within(dialog).findByText("The array is stopped, so appdata cannot be backed up or restored"),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole("textbox")).toHaveValue("jellyfin");
    expect(within(dialog).getByRole("button", { name: "Restore" })).toBeEnabled();
    expect(screen.queryByText("Appdata restore finished")).not.toBeInTheDocument();
  });

  it("cannot be dismissed while the restore request is running", async () => {
    let finish: (value: unknown) => void = () => {};
    appdataApi({ post: { "/appdata/backup/restore": () => new Promise((resolve) => (finish = resolve)) } });

    const dialog = await openRestore();
    await within(dialog).findByText("jellyfin/config");
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "jellyfin" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Restore" }));
    await waitFor(() => expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeDisabled());
    fireEvent.keyDown(dialog, { key: "Escape" });

    expect(screen.getByRole("dialog")).toBeInTheDocument();
    finish({ data: job("appdata_restore", "queued"), response: { ok: true } });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("does not start a restore for a mismatching confirmation", async () => {
    appdataApi({});

    const dialog = await openRestore();
    await within(dialog).findByText("jellyfin/config");
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "jellyfi" } });

    expect(within(dialog).getByRole("button", { name: "Restore" })).toBeDisabled();
    expect(mockPost).not.toHaveBeenCalledWith("/appdata/backup/restore", expect.anything());
  });
});
