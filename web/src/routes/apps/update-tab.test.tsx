import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { App } from "@/routes/apps/containers";
import { UpdateTab } from "@/routes/apps/update-tab";

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

const app = { id: "id-web", name: "web", image: "example/web", tag: "1.0", state: "running" } as App;
const onChanged = vi.fn();

type Answer = Promise<unknown>;

function ok<T>(data: T): Answer {
  return Promise.resolve({ data, response: { ok: true } });
}

function fail(code: string, message: string): Answer {
  return Promise.resolve({ error: { code, message }, response: { ok: false } });
}

function job(status: string, extra: Record<string, unknown> = {}) {
  return {
    id: "11111111-1111-1111-1111-111111111111",
    type: "container_update",
    class: "service",
    status,
    progress: status === "succeeded" ? 100 : null,
    resumable: false,
    cancellable: false,
    createdAt: "2026-10-01T12:00:00Z",
    ...extra,
  };
}

function updateEntry(extra: Record<string, unknown> = {}) {
  return {
    container: "web",
    image: "example/web",
    tag: "1.0",
    status: "update_available",
    kind: "new_version",
    availableTag: "1.1",
    checkedAt: "2026-10-01T06:00:00Z",
    bulkExcluded: false,
    ...extra,
  };
}

function record(extra: Record<string, unknown> = {}) {
  return {
    id: 1,
    container: "web",
    image: "example/web:1.0",
    previousImageId: "sha256:old",
    snapshotArchive: "pre-update.tar.zst",
    snapshotDestinationId: "dest-1",
    updatedAt: "2026-09-30T06:00:00Z",
    keepUntil: "2026-10-14T06:00:00Z",
    revertible: true,
    ...extra,
  };
}

type Fixture = { updates?: Answer; history?: Answer; job?: () => Answer };

function install(fixture: Fixture = {}): void {
  mockGet.mockImplementation((path: string) => {
    switch (path) {
      case "/apps/updates":
        return fixture.updates ?? ok({ available: true, updates: [updateEntry()] });
      case "/apps/updates/history":
        return fixture.history ?? ok({ available: true, records: [] });
      case "/jobs/{jobId}":
        return fixture.job ? fixture.job() : ok(job("running"));
      default:
        return fail("unexpected", path);
    }
  });
}

function renderTab() {
  return render(
    <MemoryRouter>
      <UpdateTab app={app} onChanged={onChanged} />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockGet.mockReset();
  mockPost.mockReset();
  mockPut.mockReset();
  onChanged.mockReset();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("Update tab", () => {
  it("shows the installed version against the available one, with when it was checked", async () => {
    install();
    renderTab();

    expect(await screen.findByText("1.1")).toBeInTheDocument();
    expect(screen.getByText("1.0")).toBeInTheDocument();
    expect(screen.getByText("Newer version available (1.1)")).toBeInTheDocument();
    expect(screen.getByText("Available version")).toBeInTheDocument();
    expect(screen.queryByText("Update available")).not.toBeInTheDocument();
    expect(screen.getByText(new Date("2026-10-01T06:00:00Z").toLocaleString())).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /changelog/i })).not.toBeInTheDocument();
  });

  it("names a rebuilt image under the same tag", async () => {
    install({ updates: ok({ available: true, updates: [updateEntry({ kind: "new_build", availableTag: undefined })] }) });
    renderTab();

    expect(await screen.findByText("Rebuilt image available")).toBeInTheDocument();
    expect(screen.getByText("1.0, rebuilt")).toBeInTheDocument();
  });

  it("shows a skipped check's message and does not read it as up to date", async () => {
    install({
      updates: ok({
        available: true,
        updates: [
          updateEntry({
            status: "skipped",
            kind: undefined,
            availableTag: undefined,
            message: "The registry is rate limiting requests; the next check will ask again.",
          }),
        ],
      }),
    });
    renderTab();

    expect(await screen.findByText("Check skipped")).toBeInTheDocument();
    expect(screen.getByText("The registry is rate limiting requests; the next check will ask again.")).toBeInTheDocument();
    expect(screen.queryByText("Up to date")).not.toBeInTheDocument();
  });

  it("shows a failed check's message", async () => {
    install({
      updates: ok({
        available: true,
        updates: [
          updateEntry({ status: "failed", kind: undefined, availableTag: undefined, message: "registry.example.com did not answer" }),
        ],
      }),
    });
    renderTab();

    expect(await screen.findByText("Check failed")).toBeInTheDocument();
    expect(screen.getByText("registry.example.com did not answer")).toBeInTheDocument();
  });

  it("falls back to a plain sentence when a check has no message", async () => {
    install({
      updates: ok({ available: true, updates: [updateEntry({ status: "not_checked", kind: undefined, availableTag: undefined, checkedAt: undefined })] }),
    });
    renderTab();

    expect(await screen.findByText("Not checked yet")).toBeInTheDocument();
    expect(screen.getByText(/No check has looked at this app yet/)).toBeInTheDocument();
  });

  it("offers no update for an app that is up to date", async () => {
    install({ updates: ok({ available: true, updates: [updateEntry({ status: "up_to_date", kind: undefined, availableTag: undefined })] }) });
    renderTab();

    expect(await screen.findByText("Up to date")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Update" })).toBeDisabled();
  });

  it("queues the update and follows its job", async () => {
    install({ job: () => ok(job("succeeded")) });
    mockPost.mockImplementation(() => ok(job("queued")));
    renderTab();

    fireEvent.click(await screen.findByRole("button", { name: "Update" }));

    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));
    expect(mockPost.mock.calls[0][0]).toBe("/apps/{id}/update");
    expect(mockPost.mock.calls[0][1].params.path.id).toBe("id-web");
    expect(await screen.findByText("Updating web")).toBeInTheDocument();
    expect(screen.getByRole("progressbar")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View job" })).toHaveAttribute(
      "href",
      "/jobs/11111111-1111-1111-1111-111111111111",
    );
  });

  it("refreshes the app and its update status when the job finishes", async () => {
    vi.useFakeTimers();
    install({ job: () => ok(job("succeeded")) });
    mockPost.mockImplementation(() => ok(job("queued")));
    renderTab();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    const before = mockGet.mock.calls.filter(([path]) => path === "/apps/updates").length;

    fireEvent.click(screen.getByRole("button", { name: "Update" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(onChanged).not.toHaveBeenCalled();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });

    expect(screen.getByText("The update finished.")).toBeInTheDocument();
    expect(onChanged).toHaveBeenCalledTimes(1);
    expect(mockGet.mock.calls.filter(([path]) => path === "/apps/updates").length).toBe(before + 1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  it("shows a failed update job's message", async () => {
    vi.useFakeTimers();
    install({ job: () => ok(job("failed", { error: { code: "pull_failed", message: "could not pull example/web:1.1" } })) });
    mockPost.mockImplementation(() => ok(job("queued")));
    renderTab();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    fireEvent.click(screen.getByRole("button", { name: "Update" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.queryByText(/The update did not finish/)).not.toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });

    expect(screen.getByText(/The update did not finish/)).toBeInTheDocument();
    expect(screen.getByText("could not pull example/web:1.1")).toBeInTheDocument();
  });

  it("does not claim which version the app is on when the failed job's message says the update was kept", async () => {
    vi.useFakeTimers();
    const message = "the update was kept, but the previous container could not be removed";
    install({ job: () => ok(job("failed", { error: { code: "remove_failed", message } })) });
    mockPost.mockImplementation(() => ok(job("queued")));
    renderTab();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    fireEvent.click(screen.getByRole("button", { name: "Update" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });

    expect(screen.getByText(message)).toBeInTheDocument();
    expect(screen.queryByText(/stays on the version/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Reverting again/)).not.toBeInTheDocument();
  });

  it("shows a refused update and queues nothing", async () => {
    install();
    mockPost.mockImplementation(() => fail("array_stopped", "The array is stopped."));
    renderTab();

    fireEvent.click(await screen.findByRole("button", { name: "Update" }));

    expect(await screen.findByText("The array is stopped.")).toBeInTheDocument();
    expect(screen.queryByText("Updating web")).not.toBeInTheDocument();
  });

  it("reverts only after the dialog is confirmed", async () => {
    install({ history: ok({ available: true, records: [record()] }) });
    mockPost.mockImplementation(() => ok(job("queued")));
    renderTab();

    fireEvent.click(await screen.findByRole("button", { name: "Revert" }));
    expect(mockPost).not.toHaveBeenCalled();
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/Its data is restored to how it was just before the update/)).toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole("button", { name: "Revert" }));

    await waitFor(() => expect(mockPost).toHaveBeenCalledTimes(1));
    expect(mockPost.mock.calls[0][0]).toBe("/apps/{id}/revert");
    expect(mockPost.mock.calls[0][1].params.path.id).toBe("id-web");
    expect(await screen.findByText("Reverting web")).toBeInTheDocument();
  });

  it("keeps the revert dialog open with the refusal when the revert is refused", async () => {
    install({ history: ok({ available: true, records: [record()] }) });
    mockPost.mockImplementation(() => fail("revert_unavailable", "The snapshot is gone from its destination."));
    renderTab();

    fireEvent.click(await screen.findByRole("button", { name: "Revert" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Revert" }));

    expect(await within(dialog).findByText("The snapshot is gone from its destination.")).toBeInTheDocument();
    expect(screen.queryByText("Reverting web")).not.toBeInTheDocument();
  });

  it("offers no revert when the update can no longer be reverted, or none is on record", async () => {
    install({ history: ok({ available: true, records: [record({ revertible: false })] }) });
    const view = renderTab();

    expect(await screen.findByText(/can no longer be reverted/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revert" })).not.toBeInTheDocument();
    view.unmount();

    install({ history: ok({ available: true, records: [record({ container: "other" })] }) });
    renderTab();
    expect(await screen.findByText(/there is no earlier version to go back to/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revert" })).not.toBeInTheDocument();
  });

  it("does not say there is nothing to revert when the history could not be loaded", async () => {
    install({ history: fail("internal", "history store unavailable") });
    renderTab();

    expect(await screen.findByText("history store unavailable")).toBeInTheDocument();
    expect(screen.queryByText(/there is no earlier version to go back to/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revert" })).not.toBeInTheDocument();
  });

  it("saves the bulk update opt-out", async () => {
    install();
    mockPut.mockImplementation(() => ok({ container: "web", bulkExcluded: true }));
    renderTab();

    fireEvent.click(await screen.findByRole("switch", { name: "Leave this app out of bulk updates" }));

    await waitFor(() => expect(mockPut).toHaveBeenCalledTimes(1));
    expect(mockPut.mock.calls[0][0]).toBe("/apps/{id}/update-policy");
    expect(mockPut.mock.calls[0][1]).toMatchObject({ params: { path: { id: "id-web" } }, body: { bulkExcluded: true } });
  });

  it("shows a failed opt-out save and leaves the switch where the server has it", async () => {
    install();
    mockPut.mockImplementation(() => fail("internal", "could not save"));
    renderTab();

    const toggle = await screen.findByRole("switch", { name: "Leave this app out of bulk updates" });
    fireEvent.click(toggle);

    expect(await screen.findByText("could not save")).toBeInTheDocument();
    await waitFor(() => expect(toggle).not.toBeChecked());
  });
});
