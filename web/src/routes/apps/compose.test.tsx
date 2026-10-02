import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "@/App";
import { MAX_IMPORT_BYTES } from "@/routes/apps/compose";

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

type Answer = Promise<unknown>;

function ok<T>(data: T): Answer {
  return Promise.resolve({ data, response: { ok: true } });
}

function fail(code: string, message: string): Answer {
  return Promise.resolve({ error: { code, message }, response: { ok: false } });
}

const COMPOSE = "services:\n  jellyfin:\n    image: example/jellyfin:1.0\n";
const EDITED = "services:\n  jellyfin:\n    image: example/jellyfin:1.1\n";
const JOB_ID = "11111111-1111-1111-1111-111111111111";

function stack(extra: Record<string, unknown> = {}) {
  return {
    name: "media-server",
    template: { id: "jellyfin", version: "1" },
    installedAt: "2026-09-01T08:00:00Z",
    manuallyEdited: false,
    compose: COMPOSE,
    ...extra,
  };
}

function job(status = "queued", extra: Record<string, unknown> = {}) {
  return {
    id: JOB_ID,
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

function container(name: string, extra: Record<string, unknown> = {}) {
  return {
    id: `id-${name}`,
    name,
    image: `example/${name}`,
    tag: "1.0",
    state: "running",
    status: "Up 3 hours",
    health: "none",
    ports: [],
    mounts: [],
    ...extra,
  };
}

type Fixture = {
  apps?: Record<string, unknown>[];
  getStack?: () => Answer;
  getApp?: (name: string) => Answer;
  getJob?: () => Answer;
};

function installGet(fixture: Fixture = {}): void {
  const apps = fixture.apps ?? [container("jellyfin", { stack: "media-server" })];
  mockGet.mockImplementation((path: string, options?: { params?: { path?: { id?: string; name?: string } } }) => {
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
      case "/apps":
        return ok({ available: true, apps });
      case "/apps/updates":
        return ok({ available: true, updates: [] });
      case "/apps/{id}": {
        const id = options?.params?.path?.id ?? "";
        if (fixture.getApp) {
          return fixture.getApp(id);
        }
        const found = apps.find((a) => a.id === id || a.name === id);
        return found ? ok(found) : fail("app_not_found", `no container "${id}"`);
      }
      case "/stacks/{name}":
        return fixture.getStack ? fixture.getStack() : ok(stack());
      case "/jobs/{jobId}":
        return fixture.getJob ? fixture.getJob() : ok(job());
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

function open(name = "jellyfin"): void {
  window.history.pushState({}, "", `/apps/${name}/compose`);
  render(<App />);
}

async function editor(): Promise<HTMLTextAreaElement> {
  return (await screen.findByRole("textbox", { name: "Compose file (YAML)" })) as HTMLTextAreaElement;
}

function type(box: HTMLElement, value: string): void {
  fireEvent.change(box, { target: { value } });
}

function putCalls(): { dryRun: boolean; compose: string; name: string }[] {
  return mockPut.mock.calls.map(([, options]) => ({
    name: options.params.path.name,
    dryRun: options.params.query.dryRun === true,
    compose: options.body.compose,
  }));
}

async function confirmApply(): Promise<void> {
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.click(within(dialog).getByRole("button", { name: "Apply" }));
}

beforeEach(() => {
  cleanup();
  mockGet.mockReset();
  mockPost.mockReset();
  mockPut.mockReset();
  mockMatchMedia(false);
  installGet();
  mockPut.mockImplementation((_path: string, options: { params: { query: { dryRun?: boolean } }; body: { compose: string } }) =>
    ok({ applied: options.params.query.dryRun !== true, stack: stack({ compose: options.body.compose, manuallyEdited: true }) }),
  );
  mockPost.mockImplementation(() => ok(job()));
});

afterEach(() => {
  vi.useRealTimers();
});

describe("the /apps/:name/compose route", () => {
  it("renders the editor with the stack's stored text from the application's route tree", async () => {
    open();

    const box = await editor();
    expect(box).toHaveValue(COMPOSE);
    expect(screen.getByRole("heading", { name: "Compose file of media-server" })).toBeInTheDocument();
    expect(screen.queryByText(/page is built in a later issue/i)).not.toBeInTheDocument();
    expect(mockGet).toHaveBeenCalledWith("/stacks/{name}", expect.objectContaining({ params: { path: { name: "media-server" } } }));
  });

  it("follows a container to the stack that owns it, whatever the stack is called", async () => {
    open("jellyfin");
    await editor();
    expect(mockGet).not.toHaveBeenCalledWith("/stacks/{name}", expect.objectContaining({ params: { path: { name: "jellyfin" } } }));
  });

  it("is reached from the detail page of a managed container", async () => {
    window.history.pushState({}, "", "/apps/jellyfin");
    render(<App />);

    fireEvent.click(await screen.findByRole("link", { name: "Edit the Compose file" }));

    expect(await editor()).toHaveValue(COMPOSE);
    expect(window.location.pathname).toBe("/apps/jellyfin/compose");
  });

  it("opens a stack that has no container by its own name", async () => {
    installGet({ apps: [] });
    open("media-server");
    expect(await editor()).toHaveValue(COMPOSE);
  });
});

describe("the manually edited badge", () => {
  it("shows when the server says the stack was edited by hand", async () => {
    installGet({ getStack: () => ok(stack({ manuallyEdited: true })) });
    open();
    await editor();
    expect(screen.getByText("Manually edited")).toBeInTheDocument();
  });

  it("is absent for a stack the server says was not edited", async () => {
    open();
    await editor();
    expect(screen.queryByText("Manually edited")).not.toBeInTheDocument();
  });

  it("follows the stack the server returns after an apply, not the page's own guess", async () => {
    mockPut.mockImplementation((_path: string, options: { body: { compose: string } }) =>
      ok({ applied: true, stack: stack({ compose: options.body.compose, manuallyEdited: false }) }),
    );
    open();
    type(await editor(), EDITED);
    await confirmApply();
    await screen.findByText("Starting media-server");
    expect(screen.queryByText("Manually edited")).not.toBeInTheDocument();
  });

  it("appears after an apply once the server returns it edited", async () => {
    open();
    type(await editor(), EDITED);
    await confirmApply();
    expect(await screen.findByText("Manually edited")).toBeInTheDocument();
  });
});

describe("the action toolbar", () => {
  it("is the coss Toolbar, and its roving focus moves between Validate, Import and Apply with the arrow keys", async () => {
    open();
    await editor();

    const toolbar = screen.getByRole("toolbar");
    expect(toolbar).toHaveAttribute("data-slot", "toolbar");
    const validate = within(toolbar).getByRole("button", { name: "Validate" });
    const importButton = within(toolbar).getByRole("button", { name: "Import" });
    const apply = within(toolbar).getByRole("button", { name: "Apply" });
    for (const button of [validate, importButton, apply]) {
      expect(button).toHaveAttribute("data-slot", "toolbar-button");
    }

    act(() => validate.focus());
    fireEvent.keyDown(validate, { key: "ArrowRight" });
    await waitFor(() => expect(importButton).toHaveFocus());
    fireEvent.keyDown(importButton, { key: "ArrowRight" });
    await waitFor(() => expect(apply).toHaveFocus());
    fireEvent.keyDown(apply, { key: "ArrowLeft" });
    await waitFor(() => expect(importButton).toHaveFocus());
  });

  it("disables Import and Apply, natively, while Validate is running", async () => {
    let finish: (value: unknown) => void = () => {};
    mockPut.mockImplementation(() => new Promise((resolve) => (finish = resolve)));
    open();
    await editor();

    const toolbar = screen.getByRole("toolbar");
    const validate = within(toolbar).getByRole("button", { name: "Validate" });
    const importButton = within(toolbar).getByRole("button", { name: "Import" });
    const apply = within(toolbar).getByRole("button", { name: "Apply" });
    fireEvent.click(validate);

    await waitFor(() => expect(importButton).toBeDisabled());
    expect(apply).toBeDisabled();
    expect(validate).toBeDisabled();

    await act(async () => finish(await ok({ applied: false, stack: stack() })));
    await waitFor(() => expect(importButton).toBeEnabled());
    expect(apply).toBeEnabled();
    expect(validate).toBeEnabled();
  });
});

describe("Validate", () => {
  it("checks the text with a dry run and reports it valid without storing anything", async () => {
    open();
    type(await editor(), EDITED);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));

    expect(await screen.findByText("The Compose file is valid")).toBeInTheDocument();
    expect(putCalls()).toEqual([{ name: "media-server", dryRun: true, compose: EDITED }]);
    expect(mockPost).not.toHaveBeenCalled();
    expect(screen.queryByText("Manually edited")).not.toBeInTheDocument();
  });

  it("shows the compiler's message when the file is refused, and keeps the text", async () => {
    mockPut.mockImplementation(() => fail("invalid_stack", "services.web.image: required"));
    open();
    type(await editor(), "services: nope");
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));

    expect(await screen.findByText("The Compose file is not valid")).toBeInTheDocument();
    expect(screen.getByText("services.web.image: required")).toBeInTheDocument();
    expect(await editor()).toHaveValue("services: nope");
  });

  it("does not call a request that never reached the daemon valid", async () => {
    mockPut.mockImplementation(() => Promise.reject(new Error("network down")));
    open();
    type(await editor(), EDITED);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));

    expect(await screen.findByText("Could not check the Compose file.")).toBeInTheDocument();
    expect(screen.getByText("network down")).toBeInTheDocument();
    expect(screen.queryByText("The Compose file is valid")).not.toBeInTheDocument();
  });

  it("drops a stale result as soon as the text changes", async () => {
    open();
    const box = await editor();
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    await screen.findByText("The Compose file is valid");
    type(box, EDITED);
    expect(screen.queryByText("The Compose file is valid")).not.toBeInTheDocument();
  });
});

describe("Apply", () => {
  it("asks first, says the stack is recreated, then stores the file and starts the stack, in that order", async () => {
    open();
    type(await editor(), EDITED);
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/recreated/)).toBeInTheDocument();
    expect(mockPut).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole("button", { name: "Apply" }));

    expect(await screen.findByText("Starting media-server")).toBeInTheDocument();
    expect(putCalls()).toEqual([{ name: "media-server", dryRun: false, compose: EDITED }]);
    expect(mockPost).toHaveBeenCalledWith("/stacks/{name}/start", { params: { path: { name: "media-server" } } });
    expect(mockPut.mock.invocationCallOrder[0]).toBeLessThan(mockPost.mock.invocationCallOrder[0]);
    expect(screen.getByRole("link", { name: "View job" })).toHaveAttribute("href", `/jobs/${JOB_ID}`);
    expect(screen.getByRole("progressbar")).toBeInTheDocument();
  });

  it("changes nothing when the confirmation is cancelled", async () => {
    open();
    type(await editor(), EDITED);
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(mockPut).not.toHaveBeenCalled();
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("follows the started job until it finishes", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    let polls = 0;
    installGet({ getJob: () => ok(job(++polls < 2 ? "running" : "succeeded", { progress: polls < 2 ? 40 : 100 })) });
    open();
    type(await editor(), EDITED);
    await confirmApply();

    await screen.findByText("Starting media-server");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100);
    });
    expect(await screen.findByText("The stack was started with the new Compose file.")).toBeInTheDocument();
  });

  it("reports a refused file and keeps the user's text, starting nothing", async () => {
    mockPut.mockImplementation(() => fail("invalid_stack", "yaml: line 3: mapping values are not allowed"));
    open();
    type(await editor(), "bad: [");
    await confirmApply();

    expect(await screen.findByText("Nothing was applied: the Compose file is not valid")).toBeInTheDocument();
    expect(screen.getByText("yaml: line 3: mapping values are not allowed")).toBeInTheDocument();
    expect(await editor()).toHaveValue("bad: [");
    expect(mockPost).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("says nothing was changed when the server refuses the save, with the server's message", async () => {
    mockPut.mockImplementation(() => fail("stack_action_failed", "the compose file cannot be written"));
    open();
    type(await editor(), EDITED);
    await confirmApply();

    expect(await screen.findByText("Could not store the Compose file. Nothing was changed.")).toBeInTheDocument();
    expect(screen.getByText("the compose file cannot be written")).toBeInTheDocument();
    expect(await editor()).toHaveValue(EDITED);
    expect(screen.queryByText("Could not confirm whether the Compose file was stored")).not.toBeInTheDocument();
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("says the outcome is unknown when the save got no response, reloads the stored file and starts nothing", async () => {
    mockPut.mockImplementation(() => Promise.reject(new Error("connection reset")));
    open();
    type(await editor(), EDITED);
    installGet({ getStack: () => ok(stack({ compose: EDITED })) });
    const getsBefore = mockGet.mock.calls.length;
    await confirmApply();

    expect(await screen.findByText("Could not confirm whether the Compose file was stored")).toBeInTheDocument();
    expect(await screen.findByText(/The editor now shows the file the server has/)).toBeInTheDocument();
    expect(screen.getByText("connection reset")).toBeInTheDocument();
    expect(screen.queryByText(/Nothing was changed/)).not.toBeInTheDocument();
    expect(mockGet.mock.calls.slice(getsBefore).filter(([path]) => path === "/stacks/{name}")).toHaveLength(1);
    expect(await editor()).toHaveValue(EDITED);
    expect(mockPost).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("shows the file the server has, not the unsent text, when the reload after a save with no response succeeds", async () => {
    mockPut.mockImplementation(() => Promise.reject(new Error("connection reset")));
    open();
    type(await editor(), EDITED);
    await confirmApply();

    expect(await screen.findByText(/The editor now shows the file the server has/)).toBeInTheDocument();
    expect(await editor()).toHaveValue(COMPOSE);
  });

  it("says so when the stored file cannot be reloaded, and reloads on request", async () => {
    mockPut.mockImplementation(() => Promise.reject(new Error("connection reset")));
    open();
    type(await editor(), EDITED);
    installGet({ getStack: () => Promise.reject(new Error("still down")) });
    await confirmApply();

    expect(await screen.findByText(/the file the server has now could not be loaded/)).toBeInTheDocument();
    expect(screen.queryByText(/Nothing was changed/)).not.toBeInTheDocument();
    expect(await editor()).toHaveValue(EDITED);

    installGet();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText(/The editor now shows the file the server has/)).toBeInTheDocument();
    expect(await editor()).toHaveValue(COMPOSE);
  });

  it("puts the unsent text back on request after the reload shows a different file", async () => {
    mockPut.mockImplementation(() => Promise.reject(new Error("connection reset")));
    open();
    type(await editor(), EDITED);
    await confirmApply();

    expect(await screen.findByText(/The editor now shows the file the server has/)).toBeInTheDocument();
    expect(await editor()).toHaveValue(COMPOSE);

    fireEvent.click(screen.getByRole("button", { name: "Put your edits back" }));
    expect(await editor()).toHaveValue(EDITED);
    expect(screen.getByText("You have changes that are not applied yet.")).toBeInTheDocument();
    expect(screen.queryByText("Could not confirm whether the Compose file was stored")).not.toBeInTheDocument();
  });

  it("offers nothing to put back when the server has the text that was sent", async () => {
    mockPut.mockImplementation(() => Promise.reject(new Error("connection reset")));
    open();
    type(await editor(), EDITED);
    installGet({ getStack: () => ok(stack({ compose: EDITED })) });
    await confirmApply();

    expect(await screen.findByText(/The editor now shows the file the server has/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Put your edits back" })).not.toBeInTheDocument();
  });

  it("keeps edits made after a failed reload recoverable when the reload is tried again", async () => {
    const LATER = "services:\n  jellyfin:\n    image: example/jellyfin:1.2\n";
    mockPut.mockImplementation(() => Promise.reject(new Error("connection reset")));
    open();
    type(await editor(), EDITED);
    installGet({ getStack: () => Promise.reject(new Error("still down")) });
    await confirmApply();

    expect(await screen.findByText(/the file the server has now could not be loaded/)).toBeInTheDocument();
    type(await editor(), LATER);

    installGet();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText(/The editor now shows the file the server has/)).toBeInTheDocument();
    expect(await editor()).toHaveValue(COMPOSE);

    fireEvent.click(screen.getByRole("button", { name: "Put your edits back" }));
    expect(await editor()).toHaveValue(LATER);
  });

  it("says the file was stored but the stack was not started when starting is refused, and keeps the text", async () => {
    mockPost.mockImplementation(() => fail("array_stopped", "the array is stopped"));
    open();
    type(await editor(), EDITED);
    await confirmApply();

    expect(await screen.findByText("The file was stored, but the stack was not started")).toBeInTheDocument();
    expect(screen.getByText("the array is stopped")).toBeInTheDocument();
    expect(await editor()).toHaveValue(EDITED);
    expect(screen.queryByText("Starting media-server")).not.toBeInTheDocument();
    expect(screen.queryByText("Could not store the Compose file. Nothing was changed.")).not.toBeInTheDocument();
    expect(screen.queryByRole("progressbar")).not.toBeInTheDocument();

    mockPost.mockImplementation(() => ok(job()));
    fireEvent.click(screen.getByRole("button", { name: "Try starting again" }));
    expect(await screen.findByText("Starting media-server")).toBeInTheDocument();
    expect(putCalls()).toHaveLength(1);
    expect(mockPost).toHaveBeenCalledTimes(2);
  });

  it("reports an unreachable start as not started rather than as success", async () => {
    mockPost.mockImplementation(() => Promise.reject(new Error("connection reset")));
    open();
    type(await editor(), EDITED);
    await confirmApply();

    expect(await screen.findByText("The file was stored, but the stack was not started")).toBeInTheDocument();
    expect(screen.getByText("connection reset")).toBeInTheDocument();
  });
});

describe("Import", () => {
  function importDialog(): HTMLElement {
    fireEvent.click(screen.getByRole("button", { name: "Import" }));
    return screen.getByRole("dialog");
  }

  it("replaces the editor text with pasted text and leaves it unapplied", async () => {
    open();
    await editor();
    const dialog = importDialog();
    fireEvent.change(within(dialog).getByLabelText("Or paste the file's text"), { target: { value: EDITED } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Replace editor text" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(await editor()).toHaveValue(EDITED);
    expect(screen.getByText("You have changes that are not applied yet.")).toBeInTheDocument();
    expect(mockPut).not.toHaveBeenCalled();
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("reads an uploaded file into the editor", async () => {
    open();
    await editor();
    const dialog = importDialog();
    const file = new File([EDITED], "docker-compose.yml", { type: "application/yaml" });
    fireEvent.change(within(dialog).getByLabelText("Compose file"), { target: { files: [file] } });

    await waitFor(() => expect(within(dialog).getByLabelText("Or paste the file's text")).toHaveValue(EDITED));
    fireEvent.click(within(dialog).getByRole("button", { name: "Replace editor text" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(await editor()).toHaveValue(EDITED);
    expect(mockPut).not.toHaveBeenCalled();
  });

  it("shows the catalog message, not the browser's error text, when a file cannot be read", async () => {
    const readAsText = vi.spyOn(FileReader.prototype, "readAsText").mockImplementation(function (this: FileReader) {
      this.onerror?.(new ProgressEvent("error") as ProgressEvent<FileReader>);
    });
    try {
      open();
      await editor();
      const dialog = importDialog();
      const file = new File([EDITED], "docker-compose.yml");
      fireEvent.change(within(dialog).getByLabelText("Compose file"), { target: { files: [file] } });

      expect(await within(dialog).findByText("Could not read the file.")).toBeInTheDocument();
      expect(within(dialog).getByLabelText("Or paste the file's text")).toHaveValue("");
    } finally {
      readAsText.mockRestore();
    }
  });

  it("refuses a file over the size cap without loading it", async () => {
    open();
    await editor();
    const dialog = importDialog();
    const big = new File(["a".repeat(MAX_IMPORT_BYTES + 1)], "big.yml");
    fireEvent.change(within(dialog).getByLabelText("Compose file"), { target: { files: [big] } });

    expect(await within(dialog).findByText("The Compose file is larger than 32 KiB.")).toBeInTheDocument();
    expect(within(dialog).getByLabelText("Or paste the file's text")).toHaveValue("");
    expect(within(dialog).getByRole("button", { name: "Replace editor text" })).toBeDisabled();
  });

  it("refuses pasted text over the size cap", async () => {
    open();
    await editor();
    const dialog = importDialog();
    fireEvent.change(within(dialog).getByLabelText("Or paste the file's text"), {
      target: { value: "a".repeat(MAX_IMPORT_BYTES + 1) },
    });
    expect(within(dialog).getByRole("button", { name: "Replace editor text" })).toBeDisabled();
  });

  it("changes nothing when cancelled", async () => {
    open();
    await editor();
    const dialog = importDialog();
    fireEvent.change(within(dialog).getByLabelText("Or paste the file's text"), { target: { value: EDITED } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(await editor()).toHaveValue(COMPOSE);
  });
});

describe("the unsaved-changes guard", () => {
  it("asks before in-app navigation while the editor differs from the loaded text, and stays on Keep editing", async () => {
    open();
    type(await editor(), EDITED);
    fireEvent.click(screen.getByRole("link", { name: "Back to jellyfin" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Discard changes?")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep editing" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(window.location.pathname).toBe("/apps/jellyfin/compose");
    expect(await editor()).toHaveValue(EDITED);
  });

  it("leaves when the user discards", async () => {
    open();
    type(await editor(), EDITED);
    fireEvent.click(screen.getByRole("link", { name: "Back to jellyfin" }));
    fireEvent.click(await screen.findByRole("button", { name: "Discard changes" }));

    await waitFor(() => expect(window.location.pathname).toBe("/apps/jellyfin"));
  });

  it("does not ask when nothing was changed", async () => {
    open();
    await editor();
    fireEvent.click(screen.getByRole("link", { name: "Back to jellyfin" }));

    await waitFor(() => expect(window.location.pathname).toBe("/apps/jellyfin"));
    expect(screen.queryByText("Discard changes?")).not.toBeInTheDocument();
  });

  it("does not ask once the edit is applied", async () => {
    open();
    type(await editor(), EDITED);
    await confirmApply();
    await screen.findByText("Starting media-server");
    fireEvent.click(screen.getByRole("link", { name: "Back to jellyfin" }));

    await waitFor(() => expect(window.location.pathname).toBe("/apps/jellyfin"));
    expect(screen.queryByText("Discard changes?")).not.toBeInTheDocument();
  });

  it("asks again when the text changes after an apply whose start failed", async () => {
    mockPost.mockImplementation(() => fail("array_stopped", "the array is stopped"));
    open();
    const box = await editor();
    type(box, EDITED);
    await confirmApply();
    await screen.findByText("The file was stored, but the stack was not started");
    type(box, `${EDITED}# more\n`);
    fireEvent.click(screen.getByRole("link", { name: "Back to jellyfin" }));
    expect(await screen.findByText("Discard changes?")).toBeInTheDocument();
  });

  it("warns on tab close only while the editor has unapplied edits", async () => {
    open();
    const box = await editor();
    const clean = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(clean);
    expect(clean.defaultPrevented).toBe(false);

    type(box, EDITED);
    const dirty = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(dirty);
    expect(dirty.defaultPrevented).toBe(true);
  });

  it("does not warn on tab close after the edit is applied", async () => {
    open();
    type(await editor(), EDITED);
    await confirmApply();
    await screen.findByText("Starting media-server");
    const event = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  });
});

describe("what is not a stack, and what fails to load", () => {
  it("explains that an unmanaged container has no Compose file and links back to it", async () => {
    installGet({ apps: [container("postgres")] });
    open("postgres");

    expect(await screen.findByText("No Compose file for this app")).toBeInTheDocument();
    expect(screen.getByText(/Only apps Hoserva manages as a stack have one/)).toBeInTheDocument();
    expect(screen.getAllByRole("link", { name: "Back to postgres" }).at(-1)).toHaveAttribute("href", "/apps/postgres");
    expect(screen.queryByRole("textbox", { name: "Compose file (YAML)" })).not.toBeInTheDocument();
    expect(mockGet).not.toHaveBeenCalledWith("/stacks/{name}", expect.anything());
  });

  it("explains the same for a name no stack has when the server answers 404", async () => {
    installGet({ apps: [], getStack: () => fail("stack_not_found", "no stack") });
    open("ghost");
    expect(await screen.findByText("No Compose file for this app")).toBeInTheDocument();
  });

  it("does not call a stack missing when Docker could not be asked", async () => {
    installGet({
      getApp: () => fail("docker_unavailable", "cannot connect to Docker"),
      getStack: () => fail("stack_not_found", "no stack"),
    });
    open("ghost");
    expect(await screen.findByText("cannot connect to Docker")).toBeInTheDocument();
    expect(screen.queryByText("No Compose file for this app")).not.toBeInTheDocument();
  });

  it("shows a load failure with a retry that loads the file", async () => {
    let attempts = 0;
    installGet({ getStack: () => (++attempts === 1 ? fail("internal", "database is locked") : ok(stack())) });
    open();

    expect(await screen.findByText("database is locked")).toBeInTheDocument();
    expect(screen.queryByText("No Compose file for this app")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await editor()).toHaveValue(COMPOSE);
  });

  it("shows a load failure for a rejected request", async () => {
    installGet({ getStack: () => Promise.reject(new Error("network down")) });
    open();
    expect(await screen.findByText("network down")).toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "Compose file (YAML)" })).not.toBeInTheDocument();
  });

  it("shows a load failure when the daemon returns a stack without its text", async () => {
    installGet({ getStack: () => ok(stack({ compose: undefined })) });
    open();
    expect(await screen.findByText("Could not load the Compose file.")).toBeInTheDocument();
  });
});
