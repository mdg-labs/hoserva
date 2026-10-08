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

function fail(code: string, message: string, details?: Record<string, unknown>): Answer {
  return Promise.resolve({ error: { code, message, ...(details ? { details } : {}) }, response: { ok: false } });
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

const PORT_INPUT = { name: "WEBUI_PORT", kind: "port", label: "Web interface port", value: "8096", generated: false, required: true };
const APPDATA_INPUT = {
  name: "APPDATA",
  kind: "path",
  role: "appdata",
  label: "App data folder",
  description: "Where the app keeps its settings.",
  value: "/mnt/cache/appdata",
  generated: false,
  required: true,
};
const MEDIA_INPUT = {
  name: "MEDIA",
  kind: "path",
  role: "media",
  label: "Media library",
  value: "/mnt/user/media",
  suggestions: ["/mnt/user/media", "/mnt/user/photos"],
  generated: false,
  required: true,
};
const SECRET_INPUT = { name: "DB_PASSWORD", kind: "secret", label: "Database password", generated: true, required: false };
const NAME_INPUT = { name: "SITE_NAME", kind: "string", label: "Site name", value: "", generated: false, required: true, error: "SITE_NAME needs a value" };
const NETWORKS = {
  available: true,
  networks: [
    { name: "bridge", driver: "bridge" },
    { name: "host", driver: "host" },
    { name: "lan", driver: "macvlan" },
    { name: "none", driver: "null" },
  ],
};

function plan(inputs: unknown[], extra: Record<string, unknown> = {}) {
  return {
    template: { source: "hoserva", id: "jellyfin", revision: "1" },
    title: "Jellyfin",
    name: "jellyfin",
    inputs,
    privileges: [],
    warnings: [],
    advancedAvailable: true,
    compose: "# written by the install\n" + COMPOSE,
    digest: "digest-1",
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
  networks?: () => Answer;
};

function setup(handlers: PostHandlers, template: Answer = ok(catalogTemplate())) {
  mockGet.mockImplementation((path: string) => {
    switch (path) {
      case "/catalog/{id}":
        return template;
      case "/apps/networks":
        return (handlers.networks ?? (() => ok(NETWORKS)))();
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

  it("shows the server's message on the field named in the error's details", async () => {
    setup({
      preview: (body) => {
        const values = (body as { values?: Record<string, string> }).values ?? {};
        return values.APPDATA === "relative"
          ? fail("invalid_template_input", 'template: invalid install input: APPDATA must be an absolute path, got "relative"', { input: "APPDATA" })
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
      expect.objectContaining({ body: { values: { DB_PASSWORD: "hunter2-hunter2" }, planDigest: "digest-1" } }),
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
      expect.objectContaining({ params: { path: { id: "jellyfin" } }, body: { planDigest: "digest-1" } }),
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

  it("tells the user the app changed, installs nothing and shows the new privilege summary before another install", async () => {
    let previews = 0;
    const risky = [{ kind: "privileged", service: "jellyfin", description: "Has full access to this server." }];
    setup({
      preview: () =>
        ok(previews++ === 0 ? plan([PORT_INPUT, APPDATA_INPUT]) : plan([PORT_INPUT, APPDATA_INPUT], { digest: "digest-2", privileges: risky })),
      install: (body) =>
        (body as { planDigest?: string }).planDigest === "digest-1"
          ? fail("template_changed", "template: the template changed since it was previewed")
          : ok(installed()),
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", INSTALL_BUTTON));

    expect(
      await screen.findByText("This app changed after you reviewed it, so nothing was installed. The summary below is updated: read it again before installing."),
    ).toBeInTheDocument();
    expect(await screen.findByText("Has full access to this server.")).toBeInTheDocument();
    expect(callsTo("/templates/{id}/preview")).toHaveLength(2);
    expect(callsTo("/stacks/{name}/start")).toHaveLength(0);
    await waitFor(() => expect(screen.getByRole("button", INSTALL_BUTTON)).toBeEnabled());

    fireEvent.click(screen.getByRole("button", INSTALL_BUTTON));
    await waitFor(() => expect(callsTo("/templates/{id}/install")).toHaveLength(2));
    expect(callsTo("/templates/{id}/install")[1][1]).toEqual(expect.objectContaining({ body: { planDigest: "digest-2" } }));
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
    expect(callsTo("/templates/{id}/install")[0][1]).toEqual(
      expect.objectContaining({ body: { name: "media-jelly", planDigest: "digest-1" } }),
    );
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

  it("does not read a field out of the words of an error that names no input", async () => {
    setup({
      preview: (body) => {
        const values = (body as { values?: Record<string, string> }).values ?? {};
        return values.APPDATA === "relative"
          ? fail("invalid_template_input", "template: invalid install input: APPDATA must be an absolute path")
          : ok(plan([PORT_INPUT, APPDATA_INPUT]));
      },
    });
    renderPage();

    const appdata = await screen.findByLabelText("App data folder");
    fireEvent.change(appdata, { target: { value: "relative" } });

    expect(await screen.findByText(/APPDATA must be an absolute path/)).toBeInTheDocument();
    expect(appdata).not.toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("button", INSTALL_BUTTON)).toBeDisabled();
  });

  it("lists an input that needs a value, blocks the install and marks it only once it was touched", async () => {
    setup({
      preview: (body) => {
        const values = (body as { values?: Record<string, string> }).values ?? {};
        return ok(plan([PORT_INPUT, values.SITE_NAME ? { ...NAME_INPUT, value: values.SITE_NAME, error: undefined } : NAME_INPUT]));
      },
    });
    renderPage();

    const site = await screen.findByLabelText("Site name");
    expect(screen.getByLabelText("Web interface port")).toHaveValue(8096);
    expect(screen.queryByText("Optional")).not.toBeInTheDocument();
    expect(screen.queryByText("SITE_NAME needs a value")).not.toBeInTheDocument();
    expect(screen.getByRole("button", INSTALL_BUTTON)).toBeDisabled();
    expect(screen.getByText("Fill in the settings that still need a value before installing.")).toBeInTheDocument();

    fireEvent.change(site, { target: { value: "x" } });
    fireEvent.change(site, { target: { value: "" } });
    expect(await screen.findByText("SITE_NAME needs a value")).toHaveAttribute("role", "alert");
    expect(site).toHaveAttribute("aria-invalid", "true");

    fireEvent.change(site, { target: { value: "My site" } });
    await waitFor(() => expect(screen.queryByText("SITE_NAME needs a value")).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole("button", INSTALL_BUTTON)).toBeEnabled());
  });

  it("offers bridge, the server's network and every existing network as choice cards", async () => {
    setup({});
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Advanced" }));
    expect(await screen.findByRole("radio", { name: /As the app is set up/ })).toBeChecked();
    expect(screen.getByRole("radio", { name: /^Bridge/ })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /The server's network/ })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /^lan/ })).toBeInTheDocument();
    expect(screen.getByText("An existing network (macvlan).")).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /Another network/ })).toBeInTheDocument();
    expect(screen.queryByRole("radio", { name: /^none/ })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: /^lan/ }));
    await waitFor(() =>
      expect(callsTo("/templates/{id}/preview").at(-1)?.[1]).toEqual(expect.objectContaining({ body: { networkMode: "lan" } })),
    );
  });

  it("warns before the server's own network is chosen and sends it", async () => {
    setup({});
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Advanced" }));
    expect(screen.queryByText("The app will share the server's network")).not.toBeInTheDocument();
    fireEvent.click(await screen.findByRole("radio", { name: /The server's network/ }));

    expect(screen.getByText("The app will share the server's network")).toBeInTheDocument();
    await waitFor(() =>
      expect(callsTo("/templates/{id}/preview").at(-1)?.[1]).toEqual(expect.objectContaining({ body: { networkMode: "host" } })),
    );
  });

  it("does not show a failed network list as no networks, and loads it again on request", async () => {
    let answers = 0;
    setup({ networks: () => (answers++ === 0 ? fail("internal", "the Engine did not answer") : ok(NETWORKS)) });
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Advanced" }));
    expect(await screen.findByText("Could not load the existing networks.")).toBeInTheDocument();
    expect(screen.getByText("the Engine did not answer")).toBeInTheDocument();
    expect(screen.queryByRole("radio", { name: /^lan/ })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("radio", { name: /^lan/ })).toBeInTheDocument();
    expect(screen.queryByText("Could not load the existing networks.")).not.toBeInTheDocument();
  });

  it("says why the networks are unknown when Docker is not reachable", async () => {
    setup({ networks: () => ok({ available: false, networks: [], message: "Docker is not running." }) });
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Advanced" }));
    expect(await screen.findByText("Docker is not running.")).toBeInTheDocument();
    expect(screen.getByText("Could not load the existing networks.")).toBeInTheDocument();
    expect(screen.queryByRole("radio", { name: /^lan/ })).not.toBeInTheDocument();
  });

  it("shows the command that creates a missing network, lists the warning in the review and blocks the install", async () => {
    const command = "docker network create iot";
    setup({
      preview: (body) =>
        (body as { networkMode?: string }).networkMode === "iot"
          ? ok(
              plan([PORT_INPUT, APPDATA_INPUT], {
                warnings: [{ class: "missing_network", message: "The network iot does not exist.", detail: "iot", command }],
              }),
            )
          : ok(plan([PORT_INPUT, APPDATA_INPUT])),
    });
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Advanced" }));
    fireEvent.click(await screen.findByRole("radio", { name: /Another network/ }));
    fireEvent.change(screen.getByLabelText("Network name"), { target: { value: "iot" } });

    expect(await screen.findByText("The network iot does not exist yet. Create it with this command, then check again:")).toBeInTheDocument();
    expect(screen.getAllByDisplayValue(command)).toHaveLength(2);
    expect(screen.getByText("The network iot does not exist.")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", INSTALL_BUTTON)).toBeDisabled());
    expect(screen.getByText(/Create the network first/)).toBeInTheDocument();
  });

  it("marks the network field when the install is refused because the network is missing", async () => {
    setup({ install: () => fail("network_missing", "the network iot does not exist and Hoserva does not create networks") });
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Advanced" }));
    fireEvent.click(await screen.findByRole("radio", { name: /^lan/ }));
    await waitFor(() => expect(screen.getByRole("button", INSTALL_BUTTON)).toBeEnabled());
    fireEvent.click(screen.getByRole("button", INSTALL_BUTTON));

    expect((await screen.findAllByText(/the network iot does not exist and Hoserva does not create networks/)).length).toBeGreaterThan(0);
    expect(screen.getAllByRole("alert").some((node) => node.textContent?.includes("does not create networks"))).toBe(true);
    expect(callsTo("/stacks/{name}/start")).toHaveLength(0);
  });

  it("sends the restart rule that was chosen", async () => {
    setup({});
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Advanced" }));
    fireEvent.click(await screen.findByRole("combobox", { name: "Restart rule" }));
    const option = await screen.findByRole("option", { name: "Always restart" });
    fireEvent.pointerDown(option, { pointerType: "mouse" });
    fireEvent.pointerUp(option, { pointerType: "mouse" });
    fireEvent.click(option);

    await waitFor(() =>
      expect(callsTo("/templates/{id}/preview").at(-1)?.[1]).toEqual(expect.objectContaining({ body: { restart: "always" } })),
    );
    await waitFor(() => expect(screen.getByRole("button", INSTALL_BUTTON)).toBeEnabled());
    fireEvent.click(screen.getByRole("button", INSTALL_BUTTON));
    await waitFor(() => expect(callsTo("/templates/{id}/install")).toHaveLength(1));
    expect(callsTo("/templates/{id}/install")[0][1]).toEqual(
      expect.objectContaining({ body: { restart: "always", planDigest: "digest-1" } }),
    );
  });

  it("sends the resource limits and the extra parameters", async () => {
    setup({});
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Advanced" }));
    fireEvent.change(await screen.findByLabelText("CPU limit"), { target: { value: "1.5" } });
    fireEvent.change(screen.getByLabelText("Memory limit"), { target: { value: "512" } });
    fireEvent.change(screen.getByLabelText("Extra parameters"), { target: { value: "--cap-add NET_ADMIN" } });

    await waitFor(() =>
      expect(callsTo("/templates/{id}/preview").at(-1)?.[1]).toEqual(
        expect.objectContaining({ body: { cpus: 1.5, memoryMiB: 512, extraParams: "--cap-add NET_ADMIN" } }),
      ),
    );
  });

  it("marks a limit that is not a number, sends nothing for it and holds the install", async () => {
    setup({});
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Advanced" }));
    const cpus = await screen.findByLabelText("CPU limit");
    fireEvent.change(cpus, { target: { value: "lots" } });
    fireEvent.change(screen.getByLabelText("Memory limit"), { target: { value: "1.5" } });

    expect(screen.getByText("Enter a number such as 1.5, or leave it empty.")).toHaveAttribute("role", "alert");
    expect(screen.getByText("Enter a whole number of MiB, or leave it empty.")).toHaveAttribute("role", "alert");
    expect(cpus).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("button", INSTALL_BUTTON)).toBeDisabled();
    expect(screen.getByText("Fix the resource limits under Advanced before installing.")).toBeInTheDocument();
    await new Promise((resolve) => setTimeout(resolve, 600));
    expect(callsTo("/templates/{id}/preview").at(-1)?.[1]).toEqual(expect.objectContaining({ body: {} }));
  });

  it.each([
    ["cpus", "CPU limit", "the CPU limit must be from 0.01 to 1024", "5000"],
    ["memoryMiB", "Memory limit", "the memory limit must be from 6 MiB to 16777216 MiB", "2"],
    ["extraParams", "Extra parameters", "the extra parameters set restart, which the restart setting sets too", "--restart always"],
    ["networkMode", "Network name", "\"my net\" is not a Docker network name", "my net"],
  ])("shows the refusal of %s on its own field and holds the install", async (input, label, message, typed) => {
    setup({
      preview: (body) => {
        const sent = body as Record<string, unknown>;
        return sent[input] !== undefined ? fail("invalid_template_input", `template: invalid install input: ${message}`, { input }) : ok(plan([PORT_INPUT, APPDATA_INPUT]));
      },
    });
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Advanced" }));
    if (input === "networkMode") {
      fireEvent.click(await screen.findByRole("radio", { name: /Another network/ }));
    }
    const field = await screen.findByLabelText(label);
    fireEvent.change(field, { target: { value: typed } });

    const shown = await screen.findAllByText(new RegExp(message));
    expect(shown.some((node) => node.getAttribute("role") === "alert")).toBe(true);
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("button", INSTALL_BUTTON)).toBeDisabled();
  });

  it("lists the plan's warnings in the review before anything runs, with the privileges they widen", async () => {
    setup({
      preview: () =>
        ok(
          plan([PORT_INPUT, APPDATA_INPUT], {
            warnings: [
              { class: "untranslated_flag", message: "The ExtraParams flag --no-such-flag has no Compose equivalent the converter knows.", detail: "--no-such-flag" },
              { class: "flagged_path", message: "The host path of the mount at /x is outside the pool.", detail: "/srv/x" },
              { class: "note", message: "The container uses the server's network." },
            ],
            privileges: [{ kind: "added_capabilities", service: "jellyfin", detail: "NET_ADMIN", description: "Is given extra Linux capabilities." }],
          }),
        ),
    });
    renderPage();

    expect(await screen.findByText("Not carried over: an extra parameter")).toBeInTheDocument();
    expect(screen.getByText(/--no-such-flag has no Compose equivalent/)).toBeInTheDocument();
    expect(screen.getByText("A folder outside the storage pool")).toBeInTheDocument();
    expect(screen.getByText("Note")).toBeInTheDocument();
    expect(screen.getByText("Is given extra Linux capabilities.")).toBeInTheDocument();
    expect(screen.getByRole("button", INSTALL_BUTTON)).toBeEnabled();
    expect(callsTo("/templates/{id}/install")).toHaveLength(0);
  });

  it("leaves out the controls that need one service when the app has several, and keeps the restart rule", async () => {
    setup({ preview: () => ok(plan([PORT_INPUT, APPDATA_INPUT], { advancedAvailable: false })) });
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Advanced" }));
    expect(await screen.findByText(/This app is made of several parts/)).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Restart rule" })).toBeInTheDocument();
    expect(screen.queryByLabelText("CPU limit")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Memory limit")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Extra parameters")).not.toBeInTheDocument();
    expect(screen.queryByRole("radio", { name: /^Bridge/ })).not.toBeInTheDocument();
  });
});
