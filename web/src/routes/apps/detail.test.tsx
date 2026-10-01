import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "@/App";
import { AppShell } from "@/components/patterns/app-shell";
import { AppDetailPage } from "@/routes/apps/detail";

const mockGet = vi.fn();
const mockPost = vi.fn();
const mockDelete = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
    DELETE: (...args: unknown[]) => mockDelete(...args),
  },
}));

// jsdom has no ResizeObserver, which the chart's responsive container needs
// once it has points to draw.
vi.mock("@/components/patterns/chart", () => ({
  TimeSeriesChart: ({ title }: { title: string }) => <section aria-label={title}>{title}</section>,
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

function fail(code: string, message: string) {
  return Promise.resolve({ error: { code, message }, response: { ok: false } });
}

const NOW = new Date("2026-10-01T12:00:00Z");

function container(name: string, state: string, extra: Record<string, unknown> = {}) {
  return {
    id: `id-${name}`,
    name,
    image: `example/${name}`,
    tag: "1.0",
    state,
    status: state === "running" ? "Up 3 hours" : "Exited (0) 2 days ago",
    health: "none",
    ports: [],
    mounts: [],
    ...extra,
  };
}

const jellyfin = container("jellyfin", "running", {
  stack: "media-server",
  health: "healthy",
  createdAt: "2026-09-01T08:00:00Z",
  startedAt: "2026-10-01T08:30:00Z",
  restartCount: 3,
  ports: [{ hostIP: "0.0.0.0", hostPort: 8096, containerPort: 8096, protocol: "tcp" }],
  mounts: [
    {
      source: "/mnt/cache/appdata/jellyfin",
      destination: "/config",
      mode: "rw",
      readWrite: true,
      location: { kind: "cache" },
    },
    {
      source: "/mnt/user/media",
      destination: "/data/media",
      mode: "ro",
      readWrite: false,
      location: { kind: "pool", share: "media" },
    },
    {
      source: "/mnt/disk2/scratch",
      destination: "/scratch",
      mode: "rw",
      readWrite: true,
      location: { kind: "disk", disk: 2 },
    },
    {
      source: "/var/run/docker.sock",
      destination: "/var/run/docker.sock",
      mode: "rw",
      readWrite: true,
      location: { kind: "outside" },
    },
  ],
});
const jellyfinSidecar = container("jellyfin-db", "running", { stack: "media-server" });
const portainer = container("portainer", "exited", {
  createdAt: "2026-08-01T08:00:00Z",
  restartCount: 0,
});
const postgres = container("postgres", "running", { createdAt: "2026-08-01T08:00:00Z", restartCount: 0 });

type Fixture = { apps: unknown[]; list?: unknown; doctor?: unknown };

function installGet(fixture: Fixture, overrides: Record<string, () => Promise<unknown>> = {}): void {
  mockGet.mockImplementation((path: string, options?: { params?: { path?: { id?: string } } }) => {
    const override = overrides[path];
    if (override) {
      return override();
    }
    switch (path) {
      case "/setup/status":
        return ok({ adminExists: true });
      case "/auth/session":
        return ok({ id: "1", username: "admin", role: "admin", totpEnrolled: false });
      case "/status":
        return ok({ healthy: true, summary: "OK", maintenanceMode: false });
      case "/pool":
        return ok({ mounted: true, disks: [] });
      case "/jobs":
        return ok({ jobs: [] });
      case "/doctor":
        return ok(fixture.doctor ?? { overall: "pass", checks: [] });
      case "/apps":
        return ok(fixture.list ?? { available: true, apps: fixture.apps });
      case "/apps/updates":
        return ok({ available: true, updates: [] });
      case "/apps/{id}": {
        const id = options?.params?.path?.id;
        const found = (fixture.apps as { id: string; name: string }[]).find((a) => a.id === id || a.name === id);
        return found ? ok(found) : fail("app_not_found", `no container "${id}"`);
      }
      default:
        return Promise.resolve({ data: null, response: { ok: false } });
    }
  });
}

function mockMatchMedia(matches: boolean): void {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    addEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
    matches,
    media: query,
    onchange: null,
    removeEventListener: vi.fn(),
  })) as unknown as typeof window.matchMedia;
}

function Where(): React.ReactElement {
  const location = useLocation();
  return <div data-testid="where">{location.pathname}</div>;
}

function renderPage(name: string) {
  return render(
    <MemoryRouter initialEntries={[`/apps/${name}`]}>
      <AppShell>
        <Routes>
          <Route path="/apps/:name" element={<AppDetailPage />} />
          <Route path="/apps" element={<Where />} />
          <Route path="/jobs/:jobId" element={<Where />} />
        </Routes>
      </AppShell>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  cleanup();
  mockGet.mockReset();
  mockPost.mockReset();
  mockDelete.mockReset();
  mockMatchMedia(false);
  vi.useFakeTimers({ toFake: ["Date"], now: NOW });
  mockPost.mockImplementation((_path: string, options: { params: { path: { id: string } } }) =>
    ok(container(options.params.path.id.replace("id-", ""), "running")),
  );
  mockDelete.mockImplementation(() => ok({ deletedPaths: [] }));
});

afterEach(() => {
  vi.useRealTimers();
});

describe("the /apps/:name route", () => {
  it("renders the container detail page from the application's route tree", async () => {
    installGet({ apps: [jellyfin] });
    window.history.pushState({}, "", "/apps/jellyfin");
    render(<App />);

    expect(await screen.findByRole("heading", { name: "jellyfin" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Overview" })).toBeInTheDocument();
    expect(screen.queryByText(/page is built in a later issue/i)).not.toBeInTheDocument();
  });
});

describe("Overview", () => {
  it("shows state, health, image, tag, created, uptime, restarts and the web address", async () => {
    installGet({ apps: [jellyfin] });
    renderPage("jellyfin");

    const overview = (await screen.findByText("Overview", { selector: "[data-slot=card-title]" })).closest(
      "[data-slot=card]",
    ) as HTMLElement;
    expect(within(overview).getByText("Running")).toBeInTheDocument();
    expect(within(overview).getByText("Responding")).toBeInTheDocument();
    expect(within(overview).getByText("example/jellyfin")).toBeInTheDocument();
    expect(within(overview).getByText("1.0")).toBeInTheDocument();
    expect(within(overview).getByText(new Date("2026-09-01T08:00:00Z").toLocaleString())).toBeInTheDocument();
    expect(within(overview).getByText("3 hours 30 minutes")).toBeInTheDocument();
    expect(within(overview).getByText("3")).toBeInTheDocument();
    expect(within(overview).getByRole("link", { name: "Open jellyfin on port 8096" })).toHaveAttribute(
      "href",
      "http://localhost:8096",
    );
  });

  it("names the storage each mounted folder lies on", async () => {
    installGet({ apps: [jellyfin] });
    renderPage("jellyfin");

    expect(await screen.findByText("/mnt/cache/appdata/jellyfin → /config")).toBeInTheDocument();
    const line = (source: string) => screen.getByText(new RegExp(`^${source}`)).parentElement as HTMLElement;
    expect(within(line("/mnt/cache/appdata/jellyfin")).getByText(/Cache disk · Can change files/)).toBeInTheDocument();
    expect(
      within(line("/mnt/user/media")).getByText(/Shared pool, share media — files may be on any data disk · Read only/),
    ).toBeInTheDocument();
    expect(within(line("/mnt/disk2/scratch")).getByText(/Data disk 2 · Can change files/)).toBeInTheDocument();
    expect(within(line("/var/run/docker.sock")).getByText(/Outside the array/)).toBeInTheDocument();
  });

  it("links a managed container to its stack's Compose editor and does not badge it as unmanaged", async () => {
    installGet({ apps: [jellyfin] });
    renderPage("jellyfin");

    const link = await screen.findByRole("link", { name: "Edit the Compose file" });
    expect(link).toHaveAttribute("href", "/apps/jellyfin/compose");
    expect(screen.queryByText("Not managed by Hoserva")).not.toBeInTheDocument();
  });

  it("badges a container no stack manages and offers no Compose editor for it", async () => {
    installGet({ apps: [postgres] });
    renderPage("postgres");

    expect(await screen.findAllByText("Not managed by Hoserva")).not.toHaveLength(0);
    expect(screen.queryByRole("link", { name: "Edit the Compose file" })).not.toBeInTheDocument();
  });

  it("shows a stopped container as not running and a container that never ran with no uptime or start", async () => {
    installGet({ apps: [portainer] });
    renderPage("portainer");

    expect(await screen.findByText("Not running")).toBeInTheDocument();
  });

  it("offers the five tabs, and the Config one says it is not available yet", async () => {
    installGet({ apps: [postgres] });
    renderPage("postgres");

    for (const label of ["Overview", "Logs", "Stats", "Config", "Update"]) {
      expect(await screen.findByRole("tab", { name: label })).toBeInTheDocument();
    }
    fireEvent.click(screen.getByRole("tab", { name: "Config" }));
    expect(await screen.findByText("This tab is not available yet.")).toBeInTheDocument();
  });

  it("fills the Logs, Stats and Update tabs from the application's route tree", async () => {
    installGet(
      { apps: [postgres] },
      {
        "/apps/{id}/logs": () => ok("database system is ready\n"),
        "/apps/{id}/stats": () =>
          ok({
            at: NOW.toISOString(),
            cpuPercent: 4,
            memoryBytes: 1048576,
            memoryLimitBytes: 2097152,
            networkRxBytes: 0,
            networkTxBytes: 0,
            blockReadBytes: 0,
            blockWriteBytes: 0,
          }),
        "/apps/updates": () =>
          ok({
            available: true,
            updates: [
              {
                container: "postgres",
                image: "example/postgres",
                tag: "1.0",
                status: "update_available",
                kind: "new_version",
                availableTag: "1.1",
                bulkExcluded: false,
              },
            ],
          }),
        "/apps/updates/history": () => ok({ available: true, records: [] }),
      },
    );
    window.history.pushState({}, "", "/apps/postgres");
    render(<App />);

    fireEvent.click(await screen.findByRole("tab", { name: "Logs" }));
    expect(await screen.findByText(/database system is ready/)).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Follow new output" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: "Stats" }));
    expect(await screen.findByText("Processor use")).toBeInTheDocument();
    expect(screen.queryByText(/database system is ready/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: "Update" }));
    expect(await screen.findByText("Newer version available (1.1)")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Update" })).toBeEnabled();
  });
});

describe("Actions", () => {
  it("offers only the actions the state allows: a running container cannot be started", async () => {
    installGet({ apps: [postgres] });
    renderPage("postgres");

    expect(await screen.findByRole("button", { name: "Stop" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Restart" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Recreate" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Start" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove" })).toBeDisabled();
  });

  it("explains in a tooltip why an action is disabled", async () => {
    installGet({ apps: [postgres] });
    renderPage("postgres");

    const start = await screen.findByRole("button", { name: "Start" });
    const trigger = start.parentElement as HTMLElement;
    await act(async () => {
      fireEvent.focus(trigger);
      fireEvent.mouseEnter(trigger);
      fireEvent.pointerEnter(trigger);
      fireEvent.mouseMove(trigger);
      await Promise.resolve();
    });
    expect(await screen.findByText(/already running/)).toBeInTheDocument();
  });

  it("starts a stopped container and reloads it", async () => {
    installGet({ apps: [portainer] });
    renderPage("portainer");

    fireEvent.click(await screen.findByRole("button", { name: "Start" }));

    await waitFor(() => expect(mockPost).toHaveBeenCalledWith("/apps/{id}/start", { params: { path: { id: "id-portainer" } } }));
    await waitFor(() => expect(mockGet.mock.calls.filter(([path]) => path === "/apps/{id}").length).toBeGreaterThan(1));
  });

  it("stops and restarts a running container", async () => {
    installGet({ apps: [postgres] });
    renderPage("postgres");

    fireEvent.click(await screen.findByRole("button", { name: "Stop" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledWith("/apps/{id}/stop", { params: { path: { id: "id-postgres" } } }));
    fireEvent.click(await screen.findByRole("button", { name: "Restart" }));
    await waitFor(() => expect(mockPost).toHaveBeenCalledWith("/apps/{id}/restart", { params: { path: { id: "id-postgres" } } }));
  });

  it("shows the server's message when the daemon refuses an action", async () => {
    installGet({ apps: [portainer] });
    mockPost.mockImplementation(() => fail("array_stopped", "the array is stopped — start the array first"));
    renderPage("portainer");

    fireEvent.click(await screen.findByRole("button", { name: "Start" }));

    expect(await screen.findByText("the array is stopped — start the array first", { selector: "[data-slot=alert-title]" })).toBeInTheDocument();
  });

  it("shows the reason when an action's request is rejected", async () => {
    installGet({ apps: [portainer] });
    mockPost.mockImplementation(() => Promise.reject(new Error("connection reset")));
    renderPage("portainer");

    fireEvent.click(await screen.findByRole("button", { name: "Start" }));

    expect(await screen.findByText("connection reset", { selector: "[data-slot=alert-title]" })).toBeInTheDocument();
  });

  it("queues a recreate and links to its job", async () => {
    installGet({ apps: [postgres] });
    mockPost.mockImplementation(() => ok({ id: "job-42", type: "container_recreate", status: "queued" }));
    renderPage("postgres");

    fireEvent.click(await screen.findByRole("button", { name: "Recreate" }));

    expect(mockPost).toHaveBeenCalledWith("/apps/{id}/recreate", { params: { path: { id: "id-postgres" } } });
    const link = await screen.findByRole("link", { name: "View progress" });
    expect(link).toHaveAttribute("href", "/jobs/job-42");
  });

  it("shows the server's message when a recreate is refused", async () => {
    installGet({ apps: [postgres] });
    mockPost.mockImplementation(() => fail("array_stopped", "the array is stopped — start the array first"));
    renderPage("postgres");

    fireEvent.click(await screen.findByRole("button", { name: "Recreate" }));

    expect(await screen.findByText("the array is stopped — start the array first", { selector: "[data-slot=alert-title]" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "View progress" })).not.toBeInTheDocument();
  });

  it("keeps start, stop, restart and the tabs reachable at phone width", async () => {
    mockMatchMedia(true);
    installGet({ apps: [postgres] });
    renderPage("postgres");

    expect(await screen.findByRole("button", { name: "Stop" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Start" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Restart" })).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Actions for this app" })).toHaveClass("flex-wrap");
  });
});

describe("Removing", () => {
  async function openRemove(): Promise<HTMLElement> {
    fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
    return screen.findByRole("dialog");
  }

  it("removes a stopped, unmanaged container and keeps its appdata by sending no deleteAppdata", async () => {
    installGet({ apps: [portainer] });
    renderPage("portainer");

    const dialog = await openRemove();
    expect(within(dialog).getByText("Remove portainer?")).toBeInTheDocument();
    expect(within(dialog).queryByLabelText("Type the confirmation phrase")).not.toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));

    await waitFor(() => expect(mockDelete).toHaveBeenCalledTimes(1));
    expect(mockDelete).toHaveBeenCalledWith("/apps/{id}", { params: { path: { id: "id-portainer" }, query: {} } });
    expect((await screen.findByTestId("where")).textContent).toBe("/apps");
  });

  it("needs the container's name typed before it deletes the appdata, and then sends deleteAppdata", async () => {
    installGet({ apps: [portainer] });
    renderPage("portainer");

    const dialog = await openRemove();
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Also delete the app's settings and data" }));
    const confirm = within(dialog).getByRole("button", { name: "Remove" });
    expect(confirm).toBeDisabled();
    fireEvent.click(confirm);
    expect(mockDelete).not.toHaveBeenCalled();

    const input = within(dialog).getByLabelText("Type the confirmation phrase");
    fireEvent.change(input, { target: { value: "portain" } });
    expect(confirm).toBeDisabled();
    fireEvent.change(input, { target: { value: "portainer" } });
    expect(confirm).toBeEnabled();
    fireEvent.click(confirm);

    await waitFor(() => expect(mockDelete).toHaveBeenCalledTimes(1));
    expect(mockDelete).toHaveBeenCalledWith("/apps/{id}", {
      params: { path: { id: "id-portainer" }, query: { deleteAppdata: true } },
    });
  });

  it("turns the typed confirmation off again when the appdata choice is cleared", async () => {
    installGet({ apps: [portainer] });
    renderPage("portainer");

    const dialog = await openRemove();
    const box = within(dialog).getByRole("checkbox", { name: "Also delete the app's settings and data" });
    fireEvent.click(box);
    expect(within(dialog).getByLabelText("Type the confirmation phrase")).toBeInTheDocument();
    fireEvent.click(box);
    expect(within(dialog).queryByLabelText("Type the confirmation phrase")).not.toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Remove" })).toBeEnabled();
  });

  it("removes the whole stack of a managed container, listing every container in it, with the stack's name as the phrase", async () => {
    installGet({ apps: [jellyfin, jellyfinSidecar, postgres] });
    renderPage("jellyfin");

    await screen.findByRole("heading", { name: "jellyfin" });
    const dialog = await openRemove();
    expect(within(dialog).getByText("Remove the media-server stack?")).toBeInTheDocument();
    await waitFor(() => expect(within(dialog).getByText("jellyfin-db")).toBeInTheDocument());
    expect(within(dialog).getByText("jellyfin")).toBeInTheDocument();
    expect(within(dialog).queryByText("postgres")).not.toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole("checkbox", { name: "Also delete the app's settings and data" }));
    const input = within(dialog).getByLabelText("Type the confirmation phrase");
    fireEvent.change(input, { target: { value: "jellyfin" } });
    expect(within(dialog).getByRole("button", { name: "Remove" })).toBeDisabled();
    fireEvent.change(input, { target: { value: "media-server" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));

    await waitFor(() => expect(mockDelete).toHaveBeenCalledTimes(1));
    expect(mockDelete).toHaveBeenCalledWith("/stacks/{name}", {
      params: { path: { name: "media-server" }, query: { deleteAppdata: true } },
    });
  });

  it("does not remove a stack it cannot list the containers of", async () => {
    let calls = 0;
    installGet({ apps: [jellyfin] }, {
      "/apps": () => {
        calls += 1;
        return fail("internal", "engine busy");
      },
    });
    renderPage("jellyfin");

    await screen.findByRole("heading", { name: "jellyfin" });
    const dialog = await openRemove();
    expect(await within(dialog).findByText("engine busy")).toBeInTheDocument();
    expect(calls).toBeGreaterThan(0);
    expect(within(dialog).getByRole("button", { name: "Remove" })).toBeDisabled();
    expect(mockDelete).not.toHaveBeenCalled();
  });

  it.each([
    ["app_running", "This app is still running. Stop it first, then remove it."],
    ["appdata_shared", "Another app uses the same data folder, so the data was not deleted and nothing was removed."],
    ["appdata_unavailable", "Hoserva does not know where this app's data is kept, so it cannot delete it. Nothing was removed."],
    ["array_stopped", "The array is stopped, so the data cannot be deleted right now. Start the array and try again. Nothing was removed."],
    [
      "stack_project_shared",
      "Another Compose project with the same name is running on this server, so removing the stack could take down apps that are not part of it. Nothing was removed.",
    ],
  ])("explains the %s refusal in plain language, with the server's message, and stays on the page", async (code, plain) => {
    installGet({ apps: [portainer] });
    mockDelete.mockImplementation(() => fail(code, `server says ${code}`));
    renderPage("portainer");

    const dialog = await openRemove();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));

    expect(await within(dialog).findByText(plain)).toBeInTheDocument();
    expect(within(dialog).getByText(`server says ${code}`)).toBeInTheDocument();
    expect(screen.queryByTestId("where")).not.toBeInTheDocument();
  });

  it("shows the server's message for a removal that fails any other way, and for a rejected request", async () => {
    installGet({ apps: [portainer] });
    mockDelete.mockImplementationOnce(() => fail("appdata_delete_incomplete", "container removed, appdata not fully deleted"));
    renderPage("portainer");

    const dialog = await openRemove();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    expect(await within(dialog).findByText("Could not remove this app.")).toBeInTheDocument();
    expect(within(dialog).getByText("container removed, appdata not fully deleted")).toBeInTheDocument();

    mockDelete.mockImplementationOnce(() => Promise.reject(new Error("connection reset")));
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    expect(await within(dialog).findByText("connection reset")).toBeInTheDocument();
  });

  it("offers Remove for a running container of a stack but not for a running unmanaged one", async () => {
    installGet({ apps: [jellyfin, postgres] });
    renderPage("jellyfin");
    expect(await screen.findByRole("button", { name: "Remove" })).toBeEnabled();

    cleanup();
    renderPage("postgres");
    expect(await screen.findByRole("button", { name: "Remove" })).toBeDisabled();
  });
});

describe("Error and empty states", () => {
  it("answers an unknown name with a way back to the installed apps", async () => {
    installGet({ apps: [] });
    renderPage("ghost");

    expect(await screen.findByText("App not found")).toBeInTheDocument();
    expect(screen.getByText(/no app named ghost/i)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Back to installed apps" })).toHaveAttribute("href", "/apps");
    expect(screen.queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
  });

  it("shows the Docker banner with the install command when Docker is not available", async () => {
    installGet(
      {
        apps: [],
        doctor: {
          overall: "warn",
          checks: [
            {
              id: "docker",
              name: "Docker Engine",
              status: "warn",
              message: "Docker is not installed",
              remediation: "apt-get install -y docker-ce",
            },
          ],
        },
      },
      { "/apps/{id}": () => fail("docker_unavailable", "Docker is not installed or not reachable") },
    );
    renderPage("jellyfin");

    expect(await screen.findByText("Docker is not available")).toBeInTheDocument();
    expect(screen.getByText("Docker is not installed or not reachable")).toBeInTheDocument();
    expect(await screen.findByDisplayValue("apt-get install -y docker-ce")).toBeInTheDocument();
    expect(screen.queryByText("App not found")).not.toBeInTheDocument();
  });

  it("names a failed load, with a retry, instead of an empty or missing page", async () => {
    installGet({ apps: [jellyfin] }, { "/apps/{id}": () => fail("internal", "engine socket refused the request") });
    renderPage("jellyfin");

    expect(await screen.findByText("engine socket refused the request")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
    expect(screen.queryByText("App not found")).not.toBeInTheDocument();
  });

  it("names a rejected load too", async () => {
    installGet({ apps: [jellyfin] }, { "/apps/{id}": () => Promise.reject(new Error("network down")) });
    renderPage("jellyfin");

    expect(await screen.findByText("network down")).toBeInTheDocument();
    expect(screen.queryByText("App not found")).not.toBeInTheDocument();
  });

  it("does not take a load with no body for a container", async () => {
    installGet({ apps: [jellyfin] }, { "/apps/{id}": () => Promise.resolve({ data: undefined, response: { ok: true } }) });
    renderPage("jellyfin");

    expect(await screen.findByText("Could not load this app.")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "jellyfin" })).not.toBeInTheDocument();
  });
});
