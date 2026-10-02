import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App as AppRoutes } from "@/App";
import type { App } from "@/routes/apps/containers";
import { ConfigTab } from "@/routes/apps/config-tab";

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

type Answer = Promise<unknown>;

function ok<T>(data: T): Answer {
  return Promise.resolve({ data, response: { ok: true } });
}

function fail(code: string, message: string): Answer {
  return Promise.resolve({ error: { code, message }, response: { ok: false } });
}

const JOB_ID = "11111111-1111-1111-1111-111111111111";

function job(status = "queued") {
  return {
    id: JOB_ID,
    type: "stack_start",
    class: "service",
    status,
    progress: null,
    resumable: false,
    cancellable: false,
    createdAt: "2026-10-01T12:00:00Z",
  };
}

const SECRET = "s3cr3t-value-never-shown";

function config(extra: Record<string, unknown> = {}, inputs?: unknown[]) {
  return {
    stack: {
      name: "notes",
      template: { source: "hoserva", id: "notes", revision: "3" },
      installedAt: "2026-09-01T08:00:00Z",
      manuallyEdited: false,
      ...extra,
    },
    inputs: inputs ?? [
      { name: "DB_PASSWORD", kind: "secret", label: "Database password", set: true, readOnly: false },
      { name: "GPU", kind: "device", label: "Hardware transcoding", value: "/dev/dri/renderD128", readOnly: true },
      { name: "SITE_NAME", kind: "string", label: "Site name", value: "Notes", readOnly: false },
      { name: "WEBUI_PORT", kind: "port", label: "Web interface port", value: "3000", readOnly: false },
    ],
  };
}

const managed = { id: "id-notes", name: "notes", image: "example/notes", tag: "1.0", state: "running", stack: "notes" } as App;
const unmanaged = { id: "id-pg", name: "postgres", image: "postgres", tag: "16", state: "running" } as App;

function renderTab(app: App = managed) {
  return render(
    <MemoryRouter>
      <ConfigTab app={app} />
    </MemoryRouter>,
  );
}

function putBodies(): unknown[] {
  return mockPut.mock.calls.map((call) => (call[1] as { body: unknown }).body);
}

async function applyDialog(): Promise<HTMLElement> {
  fireEvent.click(screen.getByRole("button", { name: "Apply changes" }));
  return screen.findByRole("dialog");
}

function mockMatchMedia(): void {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    addEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
    matches: false,
    media: query,
    onchange: null,
    removeEventListener: vi.fn(),
  })) as unknown as typeof window.matchMedia;
}

beforeEach(() => {
  mockMatchMedia();
  mockGet.mockReset();
  mockPost.mockReset();
  mockPut.mockReset();
  mockGet.mockImplementation((path: string) => {
    if (path === "/stacks/{name}/config") return ok(config());
    if (path === "/jobs/{jobId}") return ok(job("running"));
    return fail("unexpected", path);
  });
  mockPost.mockImplementation(() => ok(job()));
});

afterEach(cleanup);

describe("Config tab", () => {
  it("shows the stack's settings with the values it has now, a secret as set and a device as fixed", async () => {
    renderTab();

    expect(await screen.findByLabelText("Site name")).toHaveValue("Notes");
    expect(screen.getByLabelText("Web interface port")).toHaveValue(3000);
    expect(mockGet).toHaveBeenCalledWith("/stacks/{name}/config", expect.objectContaining({ params: { path: { name: "notes" } } }));
    expect(screen.getByText("A value is set. It is kept on the server and is never shown.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Replace" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Generate" })).toBeInTheDocument();
    expect(screen.getByLabelText("Hardware transcoding")).toBeDisabled();
    expect(screen.getByLabelText("Hardware transcoding")).toHaveValue("/dev/dri/renderD128");
    expect(screen.queryByText(SECRET)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Apply changes" })).toBeDisabled();
  });

  it("marks a stack whose Compose file was edited by hand", async () => {
    mockGet.mockImplementation(() => ok(config({ manuallyEdited: true })));
    renderTab();

    expect(await screen.findByText("Manually edited")).toBeInTheDocument();
    expect(screen.getByText(/never rewrites the Compose file/)).toBeInTheDocument();
  });

  it("points an unmanaged container to the Compose editor without asking the API", () => {
    renderTab(unmanaged);

    expect(screen.getByText(/is not part of an app stack that Hoserva manages/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open the Compose editor" })).toHaveAttribute("href", "/apps/postgres/compose");
    expect(mockGet).not.toHaveBeenCalled();
  });

  it("points a stack without a template to the Compose editor", async () => {
    mockGet.mockImplementation(() => fail("stack_has_no_template", "no template"));
    renderTab();

    expect(await screen.findByText(/was not installed from an app template/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open the Compose editor" })).toHaveAttribute("href", "/apps/notes/compose");
    expect(screen.queryByRole("button", { name: "Apply changes" })).not.toBeInTheDocument();
  });

  it("shows a not-found state, not the no-form note, for a stack that no longer exists", async () => {
    mockGet.mockImplementation(() => fail("stack_not_found", "no stack"));
    renderTab();

    expect(await screen.findByText("App not found")).toBeInTheDocument();
    expect(screen.getByText(/no app stack named notes on this server any more/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Back to installed apps" })).toHaveAttribute("href", "/apps");
    expect(screen.queryByText(/was not installed from an app template/)).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Open the Compose editor" })).not.toBeInTheDocument();
  });

  it("reports a settings request that failed, and tries again on request", async () => {
    mockGet.mockImplementationOnce(() => fail("stack_action_failed", "the .env cannot be opened"));
    renderTab();

    expect(await screen.findByText("Could not load the settings of this app.")).toBeInTheDocument();
    expect(screen.getByText("the .env cannot be opened")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Apply changes" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByLabelText("Site name")).toBeInTheDocument();
  });

  it("reports a settings request that never got an answer instead of an empty form", async () => {
    mockGet.mockImplementationOnce(() => Promise.reject(new Error("network down")));
    renderTab();

    expect(await screen.findByText("network down")).toBeInTheDocument();
    expect(screen.queryByLabelText("Site name")).not.toBeInTheDocument();
  });
});

describe("Applying", () => {
  it("asks first and says the app restarts, then saves only the changed values and starts the stack, in that order", async () => {
    mockPut.mockImplementation(() => ok(config({}, [
      { name: "SITE_NAME", kind: "string", label: "Site name", value: "Notes", readOnly: false },
      { name: "WEBUI_PORT", kind: "port", label: "Web interface port", value: "3100", readOnly: false },
    ])));
    renderTab();
    const port = await screen.findByLabelText("Web interface port");
    fireEvent.change(port, { target: { value: "3100" } });

    const dialog = await applyDialog();
    expect(within(dialog).getByText(/restarts and is briefly unavailable/)).toBeInTheDocument();
    expect(mockPut).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole("button", { name: "Apply changes" }));

    expect(await screen.findByText("Starting notes")).toBeInTheDocument();
    expect(mockPut).toHaveBeenCalledWith("/stacks/{name}/config", { params: { path: { name: "notes" } }, body: { values: { WEBUI_PORT: "3100" } } });
    expect(mockPost).toHaveBeenCalledWith("/stacks/{name}/start", { params: { path: { name: "notes" } } });
    expect(mockPut.mock.invocationCallOrder[0]).toBeLessThan(mockPost.mock.invocationCallOrder[0]);
    expect(screen.getByRole("progressbar")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View job" })).toHaveAttribute("href", `/jobs/${JOB_ID}`);
    expect(screen.getByLabelText("Web interface port")).toHaveValue(3100);
    expect(screen.getByRole("button", { name: "Apply changes" })).toBeDisabled();
  });

  it("changes nothing when the confirmation is cancelled", async () => {
    renderTab();
    fireEvent.change(await screen.findByLabelText("Site name"), { target: { value: "Team" } });

    const dialog = await applyDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(mockPut).not.toHaveBeenCalled();
    expect(mockPost).not.toHaveBeenCalled();
    expect(screen.getByLabelText("Site name")).toHaveValue("Team");
  });

  it("shows the server's refusal on the field it names, keeps the edits and starts nothing", async () => {
    mockPut.mockImplementation(() =>
      fail("no_free_port", "template: the port is already in use: WEBUI_PORT asks for 4000, which a container, a stack or the host already uses"),
    );
    renderTab();
    fireEvent.change(await screen.findByLabelText("Web interface port"), { target: { value: "4000" } });
    fireEvent.change(screen.getByLabelText("Site name"), { target: { value: "Team" } });

    const dialog = await applyDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply changes" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("WEBUI_PORT asks for 4000");
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(mockPost).not.toHaveBeenCalled();
    expect(screen.getByLabelText("Web interface port")).toHaveValue(4000);
    expect(screen.getByLabelText("Site name")).toHaveValue("Team");
    expect(screen.queryByText("Could not save the settings. Nothing was changed.")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Apply changes" })).toBeEnabled();
  });

  it("shows a refusal that names no field as a banner", async () => {
    mockPut.mockImplementation(() => fail("invalid_stack_env", "container: the .env defines a variable Docker reserves: PATH"));
    renderTab();
    fireEvent.change(await screen.findByLabelText("Site name"), { target: { value: "Team" } });

    const dialog = await applyDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply changes" }));

    expect(await screen.findByText("Could not save the settings. Nothing was changed.")).toBeInTheDocument();
    expect(screen.getByText(/defines a variable Docker reserves/)).toBeInTheDocument();
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("says the outcome is unknown when a save got no response, reloads the settings and does not start the stack", async () => {
    mockPut.mockImplementation(() => Promise.reject(new Error("network down")));
    renderTab();
    fireEvent.change(await screen.findByLabelText("Site name"), { target: { value: "Team" } });
    mockGet.mockImplementation((path: string) =>
      path === "/stacks/{name}/config"
        ? ok(config({}, [{ name: "SITE_NAME", kind: "string", label: "Site name", value: "Team", readOnly: false }]))
        : fail("unexpected", path),
    );
    const getsBefore = mockGet.mock.calls.length;

    const dialog = await applyDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply changes" }));

    expect(await screen.findByText("Could not confirm whether the settings were saved")).toBeInTheDocument();
    expect(await screen.findByText(/The form now shows the settings the server has/)).toBeInTheDocument();
    expect(screen.getByText("network down")).toBeInTheDocument();
    expect(screen.queryByText(/Nothing was changed/)).not.toBeInTheDocument();
    expect(mockGet.mock.calls.length).toBe(getsBefore + 1);
    expect(mockPost).not.toHaveBeenCalled();
    expect(screen.getByLabelText("Site name")).toHaveValue("Team");
    expect(screen.queryByLabelText("Web interface port")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Apply changes" })).toBeDisabled();
  });

  it("says so when the settings cannot be reloaded after a save with no response, and reloads on request", async () => {
    mockPut.mockImplementation(() => Promise.reject(new Error("network down")));
    renderTab();
    fireEvent.change(await screen.findByLabelText("Site name"), { target: { value: "Team" } });
    mockGet.mockImplementationOnce(() => Promise.reject(new Error("still down")));

    const dialog = await applyDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply changes" }));

    expect(await screen.findByText(/the settings the server has now could not be loaded/)).toBeInTheDocument();
    expect(screen.queryByText(/Nothing was changed/)).not.toBeInTheDocument();
    expect(screen.getByLabelText("Site name")).toHaveValue("Team");

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText(/The form now shows the settings the server has/)).toBeInTheDocument();
    expect(screen.getByLabelText("Site name")).toHaveValue("Notes");
  });

  it("treats an abort after the request was sent as an unknown outcome and reloads", async () => {
    mockPut.mockImplementation(() => Promise.reject(new DOMException("aborted", "AbortError")));
    renderTab();
    fireEvent.change(await screen.findByLabelText("Site name"), { target: { value: "Team" } });

    const dialog = await applyDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply changes" }));

    expect(await screen.findByText("Could not confirm whether the settings were saved")).toBeInTheDocument();
    expect(await screen.findByText(/The form now shows the settings the server has/)).toBeInTheDocument();
    expect(screen.queryByText(/Nothing was changed/)).not.toBeInTheDocument();
    expect(screen.getByLabelText("Site name")).toHaveValue("Notes");
    expect(mockPost).not.toHaveBeenCalled();
  });

  async function unknownOutcome(): Promise<void> {
    mockPut.mockImplementation(() => Promise.reject(new Error("network down")));
    renderTab();
    fireEvent.change(await screen.findByLabelText("Site name"), { target: { value: "Team" } });
    const dialog = await applyDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply changes" }));
    expect(await screen.findByText(/The form now shows the settings the server has/)).toBeInTheDocument();
  }

  it("offers to restart with the saved settings after an unknown outcome, asks first, then starts the stack and follows the job", async () => {
    await unknownOutcome();
    expect(mockPost).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Restart to apply the saved settings" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/restarts and is briefly unavailable/)).toBeInTheDocument();
    expect(mockPost).not.toHaveBeenCalled();
    fireEvent.click(within(dialog).getByRole("button", { name: "Restart to apply the saved settings" }));

    expect(await screen.findByText("Starting notes")).toBeInTheDocument();
    expect(mockPost).toHaveBeenCalledWith("/stacks/{name}/start", { params: { path: { name: "notes" } } });
    expect(mockPut).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("link", { name: "View job" })).toHaveAttribute("href", `/jobs/${JOB_ID}`);
    expect(screen.queryByRole("button", { name: "Restart to apply the saved settings" })).not.toBeInTheDocument();
  });

  it("does not offer the restart when the saved settings could not be reloaded, and offers it once they were", async () => {
    mockPut.mockImplementation(() => Promise.reject(new Error("network down")));
    renderTab();
    fireEvent.change(await screen.findByLabelText("Site name"), { target: { value: "Team" } });
    mockGet.mockImplementationOnce(() => Promise.reject(new Error("still down")));

    const dialog = await applyDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply changes" }));

    expect(await screen.findByText(/the settings the server has now could not be loaded/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restart to apply the saved settings" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("button", { name: "Restart to apply the saved settings" })).toBeInTheDocument();
  });

  it("withdraws the restart offer once the form is edited or another apply starts", async () => {
    await unknownOutcome();
    expect(screen.getByRole("button", { name: "Restart to apply the saved settings" })).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Site name"), { target: { value: "Crew" } });
    expect(screen.queryByRole("button", { name: "Restart to apply the saved settings" })).not.toBeInTheDocument();

    mockPut.mockImplementation(() => ok(config()));
    const dialog = await applyDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply changes" }));
    expect(await screen.findByText("Starting notes")).toBeInTheDocument();
    expect(screen.queryByText("Could not confirm whether the settings were saved")).not.toBeInTheDocument();
  });

  it("reports a failed restart of the saved settings, never as success, and starts again on request", async () => {
    await unknownOutcome();
    mockPost.mockImplementationOnce(() => fail("array_stopped", "the array is stopped"));

    fireEvent.click(screen.getByRole("button", { name: "Restart to apply the saved settings" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Restart to apply the saved settings" }));

    expect(await screen.findByText("The settings were saved, but the app was not restarted")).toBeInTheDocument();
    expect(screen.getByText("the array is stopped")).toBeInTheDocument();
    expect(screen.queryByText("Starting notes")).not.toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Try starting again" }));
    expect(await screen.findByText("Starting notes")).toBeInTheDocument();
    expect(mockPost).toHaveBeenCalledTimes(2);
  });

  it("reports a restart of the saved settings that was rejected", async () => {
    await unknownOutcome();
    mockPost.mockImplementationOnce(() => Promise.reject(new Error("connection reset")));

    fireEvent.click(screen.getByRole("button", { name: "Restart to apply the saved settings" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Restart to apply the saved settings" }));

    expect(await screen.findByText("The settings were saved, but the app was not restarted")).toBeInTheDocument();
    expect(screen.getByText("connection reset")).toBeInTheDocument();
    expect(screen.queryByText("Starting notes")).not.toBeInTheDocument();
  });

  it("says the settings were saved when the start fails, and starts again on request", async () => {
    mockPut.mockImplementation(() => ok(config()));
    mockPost.mockImplementationOnce(() => fail("array_stopped", "the array is stopped"));
    renderTab();
    fireEvent.change(await screen.findByLabelText("Site name"), { target: { value: "Team" } });

    const dialog = await applyDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply changes" }));

    expect(await screen.findByText("The settings were saved, but the app was not restarted")).toBeInTheDocument();
    expect(screen.getByText("the array is stopped")).toBeInTheDocument();
    expect(screen.queryByText("Starting notes")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Try starting again" }));
    expect(await screen.findByText("Starting notes")).toBeInTheDocument();
    expect(mockPost).toHaveBeenCalledTimes(2);
    expect(mockPut).toHaveBeenCalledTimes(1);
  });
});

describe("Secrets", () => {
  it("replaces a secret only with a typed value, and an empty replacement is not a change", async () => {
    mockPut.mockImplementation(() => ok(config()));
    renderTab();
    await screen.findByLabelText("Site name");

    fireEvent.click(screen.getByRole("button", { name: "Replace" }));
    expect(screen.getByRole("button", { name: "Apply changes" })).toBeDisabled();
    fireEvent.change(screen.getByLabelText("Database password"), { target: { value: "a new password" } });
    expect(screen.getByRole("button", { name: "Apply changes" })).toBeEnabled();

    const dialog = await applyDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply changes" }));
    await screen.findByText("Starting notes");
    expect(putBodies()).toEqual([{ values: { DB_PASSWORD: "a new password" } }]);
  });

  it("asks the daemon to generate a secret instead of making one up in the browser", async () => {
    mockPut.mockImplementation(() => ok(config()));
    renderTab();
    await screen.findByLabelText("Site name");

    fireEvent.click(screen.getByRole("button", { name: "Generate" }));
    expect(screen.getByText("A new value is made up when you apply. It is never shown.")).toBeInTheDocument();

    const dialog = await applyDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Apply changes" }));
    await screen.findByText("Starting notes");
    expect(putBodies()).toEqual([{ generate: ["DB_PASSWORD"] }]);
  });

  it("keeps the secret when Replace is taken back", async () => {
    renderTab();
    await screen.findByLabelText("Site name");

    fireEvent.click(screen.getByRole("button", { name: "Replace" }));
    fireEvent.change(screen.getByLabelText("Database password"), { target: { value: "typed" } });
    fireEvent.click(screen.getByRole("button", { name: "Keep the current value" }));

    expect(screen.queryByLabelText("Database password", { selector: "input" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Apply changes" })).toBeDisabled();
  });
});

describe("the /apps/:name route", () => {
  it("fills the Config tab from the application's route tree", async () => {
    mockGet.mockImplementation((path: string) => {
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
          return ok({ overall: "pass", checks: [] });
        case "/apps/{id}":
          return ok({ ...managed, health: "none", ports: [], mounts: [] });
        case "/stacks/{name}/config":
          return ok(config());
        default:
          return Promise.resolve({ data: null, response: { ok: false } });
      }
    });
    window.history.pushState({}, "", "/apps/notes");
    render(<AppRoutes />);

    fireEvent.click(await screen.findByRole("tab", { name: "Config" }));
    expect(await screen.findByLabelText("Site name")).toHaveValue("Notes");
    expect(screen.queryByText("This tab is not available yet.")).not.toBeInTheDocument();
  });
});
