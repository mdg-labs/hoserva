import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { InstallPage } from "@/routes/apps/install";

const mockGet = vi.fn();
const mockPost = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
  },
}));

type Answer = Promise<unknown>;

function ok<T>(data: T): Answer {
  return Promise.resolve({ data, response: { ok: true } });
}

function fail(code: string, message: string): Answer {
  return Promise.resolve({ error: { code, message }, response: { ok: false } });
}

const COMPOSE = "services:\n  jellyfin:\n    image: example/jellyfin:10\n    ports:\n      - ${WEBUI_PORT}:8096\n";

function catalogTemplate(extra: Record<string, unknown> = {}) {
  return {
    id: "jellyfin",
    revision: 1,
    title: "Jellyfin",
    categories: ["media"],
    docs: "https://example.org/docs",
    source: "hoserva",
    sourceKind: "curated",
    signed: true,
    compose: COMPOSE,
    privileges: [],
    ...extra,
  };
}

const PORT_INPUT = { name: "WEBUI_PORT", kind: "port", label: "Web interface port", value: "8096", generated: false };
const APPDATA_INPUT = {
  name: "APPDATA",
  kind: "path",
  role: "appdata",
  label: "App data folder",
  description: "Where the app keeps its settings.",
  value: "/mnt/cache/appdata",
  generated: false,
};
const MEDIA_INPUT = {
  name: "MEDIA",
  kind: "path",
  role: "media",
  label: "Media library",
  value: "/mnt/user/media",
  suggestions: ["/mnt/user/media", "/mnt/user/photos"],
  generated: false,
};
const SECRET_INPUT = { name: "DB_PASSWORD", kind: "secret", label: "Database password", generated: true };

function plan(inputs: unknown[], extra: Record<string, unknown> = {}) {
  return {
    template: { source: "hoserva", id: "jellyfin", revision: "1" },
    title: "Jellyfin",
    name: "jellyfin",
    inputs,
    privileges: [],
    compose: "# written by the install\n" + COMPOSE,
    ...extra,
  };
}

function job(status: string, extra: Record<string, unknown> = {}) {
  return {
    id: "22222222-2222-2222-2222-222222222222",
    type: "stack_start",
    class: "service",
    status,
    progress: status === "succeeded" ? 100 : null,
    resumable: false,
    cancellable: false,
    createdAt: "2026-10-01T12:00:00Z",
    ...extra,
  };
}

function installed(name = "jellyfin") {
  return {
    stack: { name, template: { source: "hoserva", id: "jellyfin", revision: "1" }, installedAt: "2026-10-01T12:00:00Z", manuallyEdited: false },
    plan: plan([PORT_INPUT, APPDATA_INPUT]),
  };
}

function logBytes(text: string) {
  return ok(new TextEncoder().encode(text).buffer);
}

type PostHandlers = {
  preview?: (body: unknown) => Answer;
  install?: (body: unknown) => Answer;
  start?: () => Answer;
};

function setup(handlers: PostHandlers, template: Answer = ok(catalogTemplate())) {
  mockGet.mockImplementation((path: string) => {
    switch (path) {
      case "/catalog/{id}":
        return template;
      case "/jobs/{jobId}":
        return ok(job("succeeded"));
      case "/jobs/{jobId}/log":
        return logBytes("Pulling image example/jellyfin:10\nContainer jellyfin Started\n");
      default:
        return fail("not_mocked", `unexpected GET ${path}`);
    }
  });
  mockPost.mockImplementation((path: string, init: { body?: unknown }) => {
    switch (path) {
      case "/templates/{id}/preview":
        return (handlers.preview ?? (() => ok(plan([PORT_INPUT, APPDATA_INPUT]))))(init.body);
      case "/templates/{id}/install":
        return (handlers.install ?? (() => ok(installed())))(init.body);
      case "/stacks/{name}/start":
        return (handlers.start ?? (() => ok(job("queued"))))();
      default:
        return fail("not_mocked", `unexpected POST ${path}`);
    }
  });
}

function renderPage(id = "jellyfin") {
  return render(
    <MemoryRouter initialEntries={[`/apps/install/${id}`]}>
      <Routes>
        <Route path="/apps/install/:appId" element={<InstallPage />} />
        <Route path="/apps" element={<p>installed apps</p>} />
      </Routes>
    </MemoryRouter>,
  );
}

function callsTo(path: string) {
  return mockPost.mock.calls.filter((call) => call[0] === path);
}

const INSTALL_BUTTON = { name: "Install Jellyfin" };

describe("InstallPage", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("builds the form from what the API resolved and shows the setup file expanded before anything runs", async () => {
    setup({ preview: () => ok(plan([PORT_INPUT, APPDATA_INPUT, MEDIA_INPUT, SECRET_INPUT], { privileges: [{ kind: "host_network", service: "jellyfin", description: "Shares the server's network." }] })) });
    renderPage();

    expect(await screen.findByRole("heading", { name: "Install Jellyfin" })).toBeInTheDocument();
    expect(mockPost).toHaveBeenCalledWith(
      "/templates/{id}/preview",
      expect.objectContaining({ params: { path: { id: "jellyfin" } }, body: {} }),
    );
    expect(await screen.findByLabelText("Web interface port")).toHaveValue(8096);
    expect(screen.getByLabelText("App data folder")).toHaveValue("/mnt/cache/appdata");
    expect(screen.getByText("Where the app keeps its settings.")).toBeInTheDocument();
    expect(screen.getByLabelText("Media library")).toHaveValue("/mnt/user/media");
    expect(screen.getByText("Hoserva catalog")).toBeInTheDocument();
    expect(screen.getByText("Shares the server's network.")).toBeInTheDocument();
    const code = screen.getByText((_, node) => node?.tagName === "CODE" && node.textContent?.startsWith("# written by the install") === true);
    expect(code).toBeVisible();
    expect(callsTo("/templates/{id}/install")).toHaveLength(0);
    expect(callsTo("/stacks/{name}/start")).toHaveLength(0);
  });

  it("offers the existing shares for a path and takes the chosen one", async () => {
    setup({});
    mockPost.mockImplementation((path: string) =>
      path === "/templates/{id}/preview" ? ok(plan([MEDIA_INPUT])) : fail("not_mocked", path),
    );
    renderPage();

    await screen.findByLabelText("Media library");
    fireEvent.click(screen.getByRole("button", { name: "Choose from the shares on this server" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "/mnt/user/photos" }));

    expect(screen.getByLabelText("Media library")).toHaveValue("/mnt/user/photos");
    await waitFor(() =>
      expect(mockPost).toHaveBeenCalledWith(
        "/templates/{id}/preview",
        expect.objectContaining({ body: { values: { MEDIA: "/mnt/user/photos" } } }),
      ),
    );
  });

  it("marks a taken port, blocks the install, and moves to the free port the API suggested", async () => {
    const taken = { ...PORT_INPUT, value: "8097", requestedValue: "8096" };
    setup({
      preview: (body) => {
        const values = (body as { values?: Record<string, string> }).values ?? {};
        return ok(plan([values.WEBUI_PORT === "8097" ? { ...PORT_INPUT, value: "8097" } : taken, APPDATA_INPUT]));
      },
    });
    renderPage();

    expect(await screen.findByText("Port 8096 is already used by something on this server.")).toBeInTheDocument();
    expect(screen.getByLabelText("Web interface port")).toHaveValue(8096);
    expect(screen.getByRole("button", INSTALL_BUTTON)).toBeDisabled();
    expect(screen.getByText("Choose a free port before installing.")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Use free port 8097" }));

    await waitFor(() => expect(screen.queryByText(/is already used by something/)).not.toBeInTheDocument());
    expect(screen.getByLabelText("Web interface port")).toHaveValue(8097);
    await waitFor(() => expect(screen.getByRole("button", INSTALL_BUTTON)).toBeEnabled());
    expect(mockPost).toHaveBeenCalledWith(
      "/templates/{id}/preview",
      expect.objectContaining({ body: { values: { WEBUI_PORT: "8097" } } }),
    );
  });

  it("shows the server's message on the field it names", async () => {
    setup({
      preview: (body) => {
        const values = (body as { values?: Record<string, string> }).values ?? {};
        return values.APPDATA === "relative"
          ? fail("invalid_template_input", 'template: invalid install input: APPDATA must be an absolute path, got "relative"')
          : ok(plan([PORT_INPUT, APPDATA_INPUT]));
      },
    });
    renderPage();

    const appdata = await screen.findByLabelText("App data folder");
    fireEvent.change(appdata, { target: { value: "relative" } });

    const message = await screen.findByText(/APPDATA must be an absolute path, got "relative"/);
    expect(message).toHaveAttribute("role", "alert");
    expect(appdata).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("button", INSTALL_BUTTON)).toBeDisabled();
    expect(screen.getAllByText(/must be an absolute path/)).toHaveLength(1);
  });

  it("shows a refusal that names no field as a banner with a way to ask again", async () => {
    let attempts = 0;
    setup({
      preview: (body) => {
        const values = (body as { values?: Record<string, string> }).values;
        if (values !== undefined && attempts++ === 0) {
          return fail("docker_unavailable", "Docker is not reachable.");
        }
        return ok(plan([PORT_INPUT, APPDATA_INPUT]));
      },
    });
    renderPage();

    fireEvent.change(await screen.findByLabelText("Web interface port"), { target: { value: "9000" } });
    expect(await screen.findByText("Docker is not reachable.")).toBeInTheDocument();
    expect(screen.getByRole("button", INSTALL_BUTTON)).toBeDisabled();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(screen.queryByText("Docker is not reachable.")).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole("button", INSTALL_BUTTON)).toBeEnabled());
  });

  it("does not show a form when the first answer is a refusal, and says why", async () => {
    setup({ preview: () => fail("invalid_template_input", "template: invalid install input: ADMIN_EMAIL needs a value") });
    renderPage();

    expect(await screen.findByText("Could not work out what installing this app would do.")).toBeInTheDocument();
    expect(screen.getByText(/ADMIN_EMAIL needs a value/)).toBeInTheDocument();
    expect(screen.queryByRole("button", INSTALL_BUTTON)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });

  it("masks a secret and keeps it out of the previews, sending it only with the install", async () => {
    setup({ preview: () => ok(plan([APPDATA_INPUT, SECRET_INPUT])) });
    renderPage();

    const secret = await screen.findByLabelText("Database password");
    expect(secret).toHaveAttribute("type", "password");
    expect(secret).toHaveAttribute("placeholder", "Made up for you when installed");
    fireEvent.change(secret, { target: { value: "hunter2-hunter2" } });
    await waitFor(() => expect(screen.getByRole("button", INSTALL_BUTTON)).toBeEnabled());
    // A preview made after the edit has settled.
    await new Promise((resolve) => setTimeout(resolve, 600));
    for (const call of callsTo("/templates/{id}/preview")) {
      expect(JSON.stringify(call[1])).not.toContain("hunter2");
    }

    fireEvent.click(screen.getByRole("button", INSTALL_BUTTON));
    await waitFor(() => expect(callsTo("/templates/{id}/install")).toHaveLength(1));
    expect(callsTo("/templates/{id}/install")[0][1]).toEqual(
      expect.objectContaining({ body: { values: { DB_PASSWORD: "hunter2-hunter2" } } }),
    );
  });

  it("does not ask for another preview while the form sits unchanged", async () => {
    setup({ preview: () => ok(plan([APPDATA_INPUT, SECRET_INPUT])) });
    renderPage();

    await screen.findByLabelText("Database password");
    expect(callsTo("/templates/{id}/preview")).toHaveLength(1);
    await new Promise((resolve) => setTimeout(resolve, 1500));
    expect(callsTo("/templates/{id}/preview")).toHaveLength(1);
  });

  it("installs, starts the stack as a job and follows its progress and output", async () => {
    setup({});
    renderPage();

    fireEvent.click(await screen.findByRole("button", INSTALL_BUTTON));

    expect(await screen.findByText("jellyfin is installed.")).toBeInTheDocument();
    expect(callsTo("/templates/{id}/install")[0][1]).toEqual(
      expect.objectContaining({ params: { path: { id: "jellyfin" } }, body: {} }),
    );
    await waitFor(() => expect(callsTo("/stacks/{name}/start")).toHaveLength(1));
    expect(callsTo("/stacks/{name}/start")[0][1]).toEqual({ params: { path: { name: "jellyfin" } } });
    expect(await screen.findByText(/Container jellyfin Started/)).toBeInTheDocument();
    expect(await screen.findByText("jellyfin is running.")).toBeInTheDocument();
    expect(screen.queryByRole("button", INSTALL_BUTTON)).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View job" })).toHaveAttribute("href", "/jobs/22222222-2222-2222-2222-222222222222");
    expect(screen.getByRole("link", { name: "Go to Installed apps" })).toHaveAttribute("href", "/apps");
  });

  it("says the app is installed but not running when the start is refused, and offers no second install", async () => {
    let starts = 0;
    setup({
      start: () => (starts++ === 0 ? fail("array_stopped", "The array is stopped.") : ok(job("queued"))),
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", INSTALL_BUTTON));

    expect(await screen.findByText("jellyfin is installed but not running")).toBeInTheDocument();
    expect(screen.getByText("The array is stopped.")).toBeInTheDocument();
    expect(screen.queryByRole("button", INSTALL_BUTTON)).not.toBeInTheDocument();
    expect(callsTo("/templates/{id}/install")).toHaveLength(1);

    fireEvent.click(screen.getByRole("button", { name: "Try starting again" }));
    expect(await screen.findByText("jellyfin is running.")).toBeInTheDocument();
    expect(callsTo("/stacks/{name}/start")).toHaveLength(2);
    expect(callsTo("/templates/{id}/install")).toHaveLength(1);
  });

  it("says the app is installed but did not start when the job fails, with the job's output", async () => {
    setup({});
    mockGet.mockImplementation((path: string) => {
      switch (path) {
        case "/catalog/{id}":
          return ok(catalogTemplate());
        case "/jobs/{jobId}":
          return ok(job("failed", { error: { code: "stack_action_failed", message: "docker compose up failed" } }));
        case "/jobs/{jobId}/log":
          return logBytes("pull access denied for example/jellyfin\n");
        default:
          return fail("not_mocked", path);
      }
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", INSTALL_BUTTON));

    expect(await screen.findByText("jellyfin is installed but did not start")).toBeInTheDocument();
    expect(screen.getByText("docker compose up failed")).toBeInTheDocument();
    expect(await screen.findByText(/pull access denied/)).toBeInTheDocument();
    expect(screen.queryByText("jellyfin is running.")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", INSTALL_BUTTON)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try starting again" })).toBeInTheDocument();
  });

  it("stops following when the job cannot be read instead of showing it as finished", async () => {
    setup({});
    mockGet.mockImplementation((path: string) => {
      switch (path) {
        case "/catalog/{id}":
          return ok(catalogTemplate());
        case "/jobs/{jobId}":
          return fail("internal", "the daemon lost the job");
        default:
          return fail("not_mocked", path);
      }
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", INSTALL_BUTTON));

    expect(await screen.findByText("the daemon lost the job")).toBeInTheDocument();
    expect(screen.queryByText("jellyfin is running.")).not.toBeInTheDocument();
  });

  it("keeps the form, with the server's reason, when the install is refused", async () => {
    setup({ install: () => fail("stack_exists", 'a stack named "jellyfin" already exists') });
    renderPage();

    fireEvent.click(await screen.findByRole("button", INSTALL_BUTTON));

    expect(await screen.findByText('a stack named "jellyfin" already exists')).toBeInTheDocument();
    expect(callsTo("/stacks/{name}/start")).toHaveLength(0);
    expect(screen.getByRole("button", INSTALL_BUTTON)).toBeEnabled();

    fireEvent.click(screen.getByRole("tab", { name: "Advanced" }));
    expect(screen.getByLabelText("Name of the app's setup")).toHaveAttribute("aria-invalid", "true");
  });

  it("lets a renamed stack through to the install", async () => {
    setup({});
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Advanced" }));
    fireEvent.change(screen.getByLabelText("Name of the app's setup"), { target: { value: " media-jelly " } });
    await waitFor(() => expect(callsTo("/templates/{id}/preview").at(-1)?.[1]).toEqual(expect.objectContaining({ body: { name: "media-jelly" } })));
    await waitFor(() => expect(screen.getByRole("button", INSTALL_BUTTON)).toBeEnabled());
    fireEvent.click(screen.getByRole("button", INSTALL_BUTTON));

    await waitFor(() => expect(callsTo("/templates/{id}/install")).toHaveLength(1));
    expect(callsTo("/templates/{id}/install")[0][1]).toEqual(expect.objectContaining({ body: { name: "media-jelly" } }));
  });

  it("sends one install however fast the button is pressed", async () => {
    let release: (value: unknown) => void = () => undefined;
    setup({
      install: () =>
        new Promise((resolve) => {
          release = resolve;
        }),
    });
    renderPage();

    const button = await screen.findByRole("button", INSTALL_BUTTON);
    fireEvent.click(button);
    fireEvent.click(button);
    fireEvent.click(button);
    expect(callsTo("/templates/{id}/install")).toHaveLength(1);

    release({ data: installed(), response: { ok: true } });
    expect(await screen.findByText("jellyfin is installed.")).toBeInTheDocument();
    expect(callsTo("/templates/{id}/install")).toHaveLength(1);
    expect(callsTo("/stacks/{name}/start")).toHaveLength(1);
  });

  it("tells the user to check Installed apps when the install request itself is lost", async () => {
    setup({ install: () => Promise.reject(new Error("network down")) });
    renderPage();

    fireEvent.click(await screen.findByRole("button", INSTALL_BUTTON));

    expect(await screen.findByText(/network down/)).toBeInTheDocument();
    expect(screen.getByText(/check Installed apps before trying again/)).toBeInTheDocument();
    expect(callsTo("/stacks/{name}/start")).toHaveLength(0);
  });

  it("marks an unsigned user-added template before anything is installed", async () => {
    setup(
      {},
      ok(catalogTemplate({ id: "quickpaste", title: "Quick Paste", source: "src-1", sourceKind: "user_added", signed: false })),
    );
    renderPage("quickpaste");

    expect(await screen.findByText("Added by you, unsigned")).toBeInTheDocument();
    expect(screen.getByText(/It has no signature, so nothing confirms who wrote it/)).toBeInTheDocument();
  });

  it("says an unknown app is not in the catalog", async () => {
    setup({}, fail("template_not_found", "no such template"));
    renderPage("nothing");

    expect(await screen.findByText('"nothing" is not in the catalog')).toBeInTheDocument();
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("says plainly when the catalog entry cannot be loaded, with a retry", async () => {
    setup({}, fail("catalog_unavailable", "The catalog is not installed."));
    renderPage();

    expect(await screen.findByText("The catalog is not installed.")).toBeInTheDocument();
    expect(within(document.body).getByRole("button", { name: "Try again" })).toBeInTheDocument();
    expect(mockPost).not.toHaveBeenCalled();
  });
});
