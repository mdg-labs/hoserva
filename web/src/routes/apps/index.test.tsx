import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "@/App";
import { AppShell } from "@/components/patterns/app-shell";
import { AppsPage } from "@/routes/apps/index";

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

type Fixture = {
  apps: unknown;
  updates: unknown;
  doctor: unknown;
};

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
  ports: [{ hostIP: "0.0.0.0", hostPort: 8096, containerPort: 8096, protocol: "tcp" }],
});
const postgres = container("postgres", "running");
const portainer = container("portainer", "exited");

function ok<T>(data: T) {
  return Promise.resolve({ data, response: { ok: true } });
}

function fail(code: string, message: string) {
  return Promise.resolve({ error: { code, message }, response: { ok: false } });
}

function defaultFixture(): Fixture {
  return {
    apps: { available: true, apps: [jellyfin, postgres, portainer] },
    updates: {
      available: true,
      updates: [
        {
          container: "jellyfin",
          image: "example/jellyfin",
          tag: "1.0",
          status: "update_available",
          kind: "new_version",
          availableTag: "2.0",
        },
        { container: "postgres", image: "example/postgres", tag: "1.0", status: "up_to_date" },
        {
          container: "portainer",
          image: "example/portainer",
          tag: "1.0",
          status: "skipped",
          message: "the registry is rate limiting requests",
        },
      ],
    },
    doctor: { overall: "pass", checks: [] },
  };
}

function installGet(fixture: Fixture, overrides: Record<string, () => Promise<unknown>> = {}): void {
  mockGet.mockImplementation((path: string) => {
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
        return ok(fixture.doctor);
      case "/apps":
        return ok(fixture.apps);
      case "/apps/updates":
        return ok(fixture.updates);
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

function renderPage() {
  return render(
    <MemoryRouter>
      <AppShell>
        <AppsPage />
      </AppShell>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  cleanup();
  mockGet.mockReset();
  mockPost.mockReset();
  mockMatchMedia(false);
  mockPost.mockImplementation((_path: string, options: { params: { path: { id: string } } }) =>
    ok(container(options.params.path.id.replace("id-", ""), "running")),
  );
});

describe("the /apps route", () => {
  it("renders the Installed page from the application's route tree", async () => {
    installGet(defaultFixture());
    window.history.pushState({}, "", "/apps");
    render(<App />);

    expect(await screen.findByRole("heading", { name: "Installed apps" })).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "jellyfin" })).toBeInTheDocument();
    expect(screen.queryByText(/not available yet/i)).not.toBeInTheDocument();
  });
});

describe("Installed apps", () => {
  it("labels an update as available only for update_available and never reads other results as up to date", async () => {
    installGet(defaultFixture());
    renderPage();

    expect(await screen.findByText("Newer version available (2.0)")).toBeInTheDocument();
    expect(screen.getByText("Not checked")).toBeInTheDocument();
    expect(screen.getByText("the registry is rate limiting requests")).toBeInTheDocument();
    expect(screen.queryByText(/up to date/i)).not.toBeInTheDocument();
  });

  it("says nothing about updates, and names the failure, when the update status cannot be loaded", async () => {
    installGet(defaultFixture(), { "/apps/updates": () => fail("internal", "registry index unreachable") });
    renderPage();

    expect(await screen.findByText("registry index unreachable")).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "jellyfin" })).toBeInTheDocument();
    expect(screen.queryByText("Not checked")).not.toBeInTheDocument();
    expect(screen.queryByText(/Newer version/)).not.toBeInTheDocument();
  });

  it("links a published web port through the address the page was opened on", async () => {
    installGet(defaultFixture());
    renderPage();

    const link = await screen.findByRole("link", { name: "Open jellyfin on port 8096" });
    expect(link).toHaveAttribute("href", "http://localhost:8096");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
  });

  it("badges exactly the containers the API reports no stack for as not managed by Hoserva", async () => {
    installGet(defaultFixture());
    renderPage();

    await screen.findByRole("link", { name: "jellyfin" });
    const badges = screen.getAllByText("Not managed by Hoserva");
    expect(badges).toHaveLength(2);
    for (const name of ["postgres", "portainer"]) {
      const row = screen.getByRole("link", { name }).closest("tr");
      expect(row).not.toBeNull();
      expect(within(row as HTMLElement).getByText("Not managed by Hoserva")).toBeInTheDocument();
    }
    const jellyfinRow = screen.getByRole("link", { name: "jellyfin" }).closest("tr");
    expect(within(jellyfinRow as HTMLElement).queryByText("Not managed by Hoserva")).not.toBeInTheDocument();
  });

  it("shows the empty state when Docker has no containers", async () => {
    installGet({ ...defaultFixture(), apps: { available: true, apps: [] } });
    renderPage();

    expect(await screen.findByText("No apps yet")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Browse the catalog" })).toHaveAttribute("href", "/apps/catalog");
    expect(screen.queryByRole("button", { name: "Start all" })).not.toBeInTheDocument();
  });

  it("names a failed load instead of showing the empty state", async () => {
    installGet(defaultFixture(), { "/apps": () => fail("internal", "engine socket refused the request") });
    renderPage();

    expect(await screen.findByText("engine socket refused the request")).toBeInTheDocument();
    expect(screen.queryByText("No apps yet")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });

  it("names a rejected load too", async () => {
    installGet(defaultFixture(), { "/apps": () => Promise.reject(new Error("network down")) });
    renderPage();

    expect(await screen.findByText("network down")).toBeInTheDocument();
    expect(screen.queryByText("No apps yet")).not.toBeInTheDocument();
  });

  it("shows the Docker banner with the install commands from the doctor check when Docker is unavailable", async () => {
    installGet({
      apps: { available: false, message: "Docker is not installed — Apps will not be available", apps: [] },
      updates: { available: false, updates: [] },
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
    });
    renderPage();

    expect(await screen.findByText("Docker is not available")).toBeInTheDocument();
    expect(screen.getByText("Docker is not installed — Apps will not be available")).toBeInTheDocument();
    expect(await screen.findByDisplayValue("apt-get install -y docker-ce")).toBeInTheDocument();
    expect(screen.queryByText("No apps yet")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Stop all" })).not.toBeInTheDocument();
  });

  it("starts a stopped container and refreshes the list", async () => {
    installGet(defaultFixture());
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Start portainer" }));

    await waitFor(() => expect(mockPost).toHaveBeenCalledWith("/apps/{id}/start", { params: { path: { id: "id-portainer" } } }));
    await waitFor(() => expect(mockGet.mock.calls.filter(([path]) => path === "/apps").length).toBeGreaterThan(1));
  });

  it("names the container and the reason when an action fails", async () => {
    installGet(defaultFixture());
    mockPost.mockImplementation(() => fail("array_stopped", "the array is stopped — start the array first"));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Start portainer" }));

    expect(await screen.findByText("1 app could not be changed")).toBeInTheDocument();
    expect(screen.getByText("portainer: the array is stopped — start the array first")).toBeInTheDocument();
  });

  it("names the container when an action's request is rejected", async () => {
    installGet(defaultFixture());
    mockPost.mockImplementation(() => Promise.reject(new Error("connection reset")));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Stop postgres" }));

    expect(await screen.findByText("postgres: connection reset")).toBeInTheDocument();
  });

  it("restarts a running container from its menu", async () => {
    installGet(defaultFixture());
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "More actions for jellyfin" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Restart" }));

    await waitFor(() => expect(mockPost).toHaveBeenCalledWith("/apps/{id}/restart", { params: { path: { id: "id-jellyfin" } } }));
  });

  it("stops every running container only after confirmation and reports the ones that failed", async () => {
    installGet(defaultFixture());
    mockPost.mockImplementation((_path: string, options: { params: { path: { id: string } } }) =>
      options.params.path.id === "id-postgres"
        ? fail("internal", "engine busy")
        : ok(container("jellyfin", "exited")),
    );
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Stop all" }));
    expect(mockPost).not.toHaveBeenCalled();

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Stop all 2 running apps?")).toBeInTheDocument();
    expect(within(dialog).getByText("jellyfin")).toBeInTheDocument();
    expect(within(dialog).getByText("postgres")).toBeInTheDocument();
    expect(within(dialog).queryByText("portainer")).not.toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop all" }));

    expect(await screen.findByText("postgres: engine busy")).toBeInTheDocument();
    expect(mockPost).toHaveBeenCalledTimes(2);
    expect(mockPost).toHaveBeenCalledWith("/apps/{id}/stop", { params: { path: { id: "id-jellyfin" } } });
    expect(mockPost).toHaveBeenCalledWith("/apps/{id}/stop", { params: { path: { id: "id-postgres" } } });
  });

  it("offers no bulk update action, because applying updates is not available yet", async () => {
    installGet(defaultFixture());
    renderPage();

    await screen.findByRole("link", { name: "jellyfin" });
    expect(screen.queryByRole("button", { name: /update/i })).not.toBeInTheDocument();
  });

  it("shows a container's recent logs as text", async () => {
    installGet(defaultFixture(), { "/apps/{id}/logs": () => ok("[jellyfin] starting\n[jellyfin] ready\n") });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Show logs of jellyfin" }));

    expect(await screen.findByText(/\[jellyfin\] ready/)).toBeInTheDocument();
    expect(mockGet).toHaveBeenCalledWith(
      "/apps/{id}/logs",
      expect.objectContaining({ params: { path: { id: "id-jellyfin" }, query: { tail: 200 } }, parseAs: "text" }),
    );
  });

  it("names a failed log load instead of showing an empty log", async () => {
    installGet(defaultFixture(), { "/apps/{id}/logs": () => fail("app_not_found", "no container \"jellyfin\"") });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Show logs of jellyfin" }));

    expect(await screen.findByText('no container "jellyfin"')).toBeInTheDocument();
    expect(screen.queryByText(/No log output/)).not.toBeInTheDocument();
    expect(screen.queryByText("This app has not written any output yet.")).not.toBeInTheDocument();
  });

  it("starts in the card view at phone width, with start, stop and logs reachable", async () => {
    mockMatchMedia(true);
    installGet(defaultFixture());
    renderPage();

    expect(await screen.findByRole("button", { name: "Stop jellyfin" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Start portainer" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Show logs of portainer" })).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("switches between the table and the card view", async () => {
    installGet(defaultFixture());
    renderPage();

    expect(await screen.findByRole("table")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("radio", { name: "Cards" }));
    await waitFor(() => expect(screen.queryByRole("table")).not.toBeInTheDocument());
    expect(screen.getByRole("link", { name: "jellyfin" })).toBeInTheDocument();
  });
});
