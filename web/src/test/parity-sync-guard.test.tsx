import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { AppShell } from "@/components/patterns/app-shell";
import { ParityPage } from "@/routes/storage/parity";

const mockGet = vi.fn();
const mockPost = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    POST: (...args: unknown[]) => mockPost(...args),
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

const JOB_ID = "a1a1a1a1-0670-4000-8000-000000000001";
const FRESH_JOB_ID = "a1a1a1a1-0670-4000-8000-000000000002";
const LATER_JOB_ID = "a1a1a1a1-0670-4000-8000-000000000003";
const GUARD_SUMMARY = "A mass deletion was detected: 40 files removed.";

// What the daemon records on the sync job the engine refused: the
// GuardBlockedError text, as RunSync returns it (internal/parity/guard.go).
function guardMessage(removed: number): string {
  return `parity: threshold guard blocked the sync: removed=${removed} (threshold-relevant), removed+updated=40.0%, triggers=[removed_count]`;
}

function job(id: string, status: string, error?: { code: string; message: string }): Record<string, unknown> {
  return {
    id,
    type: "sync",
    class: "parity",
    status,
    progress: null,
    resumable: false,
    cancellable: true,
    createdAt: "2026-01-01T00:00:00Z",
    ...(status === "queued" || status === "running" ? {} : { finishedAt: "2026-01-01T00:00:05Z" }),
    ...(error ? { error } : {}),
  };
}

function queuedJob(id = JOB_ID): Record<string, unknown> {
  return job(id, "queued");
}

function guardBlockedJob(id = JOB_ID, removed = 40): Record<string, unknown> {
  return job(id, "failed", { code: "job_failed", message: guardMessage(removed) });
}

function succeededJob(id = JOB_ID): Record<string, unknown> {
  return job(id, "succeeded");
}

function mockParityApi(guard: Record<string, unknown>, jobResult: (jobId: string) => unknown): void {
  mockGet.mockImplementation((path: string, init?: { params?: { path?: { jobId?: string } } }) => {
    switch (path) {
      case "/status":
        return Promise.resolve({
          data: { healthy: true, summary: "OK", arrayDegraded: false, parityBlocked: false, activeJobs: 0 },
          response: { ok: true },
        });
      case "/pool":
        return Promise.resolve({ data: { mounted: true, disks: [] }, response: { ok: true } });
      case "/jobs":
        return Promise.resolve({ data: { jobs: [] }, response: { ok: true } });
      case "/doctor":
        return Promise.resolve({ data: { overall: "ok", checks: [] }, response: { ok: true } });
      case "/parity":
        return Promise.resolve({ data: { freshness: "green", guard }, response: { ok: true } });
      case "/jobs/{jobId}":
        return Promise.resolve(jobResult(init?.params?.path?.jobId ?? ""));
      default:
        return Promise.resolve({ data: null, response: { ok: false } });
    }
  });
}

// Each POST /parity/sync queues the next job id, in order.
function queueSyncJobs(...ids: string[]): void {
  mockPost.mockReset();
  ids.forEach((id) => mockPost.mockResolvedValueOnce({ data: queuedJob(id), response: { ok: true } }));
}

function syncPosts(): { path: string; body: { confirm?: boolean; dryRun?: boolean } }[] {
  return mockPost.mock.calls
    .filter((call) => call[0] === "/parity/sync")
    .map((call) => ({ path: call[0] as string, body: (call[1] as { body: { confirm?: boolean } }).body }));
}

function syncConfirms(): (boolean | undefined)[] {
  return syncPosts().map((call) => call.body.confirm);
}

function renderParityPage(): void {
  render(
    <MemoryRouter>
      <AppShell>
        <ParityPage />
      </AppShell>
    </MemoryRouter>,
  );
}

async function openSyncDialog(): Promise<HTMLElement> {
  fireEvent.click(await screen.findByRole("button", { name: "Sync now" }));
  return screen.findByRole("dialog");
}

async function syncNow(): Promise<void> {
  const dialog = await openSyncDialog();
  fireEvent.click(within(dialog).getByRole("button", { name: "Sync now" }));
}

async function openOverride(): Promise<HTMLElement> {
  fireEvent.click(await screen.findByRole("button", { name: "Sync anyway…" }));
  return screen.findByRole("dialog");
}

async function closeOverride(override: HTMLElement): Promise<void> {
  fireEvent.click(within(override).getByRole("button", { name: "Close" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
}

describe("parity page sync and the threshold guard", () => {
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

  it("posts Sync now without confirm and shows the guard's block when the engine refuses the job", async () => {
    mockParityApi({ wouldBlock: false }, (id) => guardBlockedJobResult(id));
    queueSyncJobs(JOB_ID);

    renderParityPage();
    await syncNow();

    expect(await screen.findByText("Threshold guard is blocking sync")).toBeInTheDocument();
    expect(await screen.findByText(/removed=40 \(threshold-relevant\)/)).toBeInTheDocument();
    expect(screen.getByText("Guard tripped")).toBeInTheDocument();
    expect(screen.queryByText("Sync started")).not.toBeInTheDocument();
    expect(screen.queryByText("Sync finished")).not.toBeInTheDocument();
    expect(syncPosts()).toEqual([{ path: "/parity/sync", body: { confirm: false, dryRun: false } }]);
  });

  it("shows a finished sync only when the job succeeded", async () => {
    mockParityApi({ wouldBlock: false }, (id) => ({ data: succeededJob(id), response: { ok: true } }));
    queueSyncJobs(JOB_ID);

    renderParityPage();
    await syncNow();

    expect(await screen.findByText("Sync finished")).toBeInTheDocument();
    expect(screen.queryByText("Threshold guard is blocking sync")).not.toBeInTheDocument();
    expect(syncConfirms()).toEqual([false]);
  });

  it("does not report success when the request answers with an error", async () => {
    mockParityApi({ wouldBlock: false }, (id) => ({ data: succeededJob(id), response: { ok: true } }));
    mockPost.mockResolvedValue({
      error: { code: "on_battery", message: "Parity writes are held while the server is on battery." },
      response: { ok: false },
    });

    renderParityPage();
    await syncNow();

    expect(await screen.findByText("Parity writes are held while the server is on battery.")).toBeInTheDocument();
    expect(screen.queryByText("Sync started")).not.toBeInTheDocument();
    expect(screen.queryByText("Sync finished")).not.toBeInTheDocument();
    expect(syncConfirms()).toEqual([false]);
  });

  it("does not report success when the request rejects", async () => {
    mockParityApi({ wouldBlock: false }, (id) => ({ data: succeededJob(id), response: { ok: true } }));
    mockPost.mockRejectedValue(new Error("network down"));

    renderParityPage();
    await syncNow();

    expect(await screen.findByText("network down")).toBeInTheDocument();
    expect(screen.queryByText("Sync started")).not.toBeInTheDocument();
    expect(screen.queryByText("Sync finished")).not.toBeInTheDocument();
  });

  it("shows the job failure instead of a success when following the job fails", async () => {
    mockParityApi({ wouldBlock: false }, () => ({
      error: { code: "job_not_found", message: "No such job." },
      response: { ok: false },
    }));
    queueSyncJobs(JOB_ID);

    renderParityPage();
    await syncNow();

    expect(await screen.findByText("No such job.")).toBeInTheDocument();
    expect(screen.queryByText("Sync finished")).not.toBeInTheDocument();
  });

  it("posts confirm false from Sync now even when the cached guard reads tripped, and shows the cached summary as information", async () => {
    mockParityApi({ wouldBlock: true, summary: GUARD_SUMMARY }, (id) => guardBlockedJobResult(id));
    queueSyncJobs(JOB_ID);

    renderParityPage();
    const dialog = await openSyncDialog();
    expect(within(dialog).getByText("Queue a parity sync job through the threshold guard.")).toBeInTheDocument();
    expect(within(dialog).getByText(GUARD_SUMMARY)).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Sync anyway" })).not.toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Sync now" }));

    expect(await screen.findByText(/removed=40 \(threshold-relevant\)/)).toBeInTheDocument();
    expect(syncPosts()).toEqual([{ path: "/parity/sync", body: { confirm: false, dryRun: false } }]);
  });

  it("asks the guard again when Sync anyway is opened and confirms only the block that refusal returns", async () => {
    mockParityApi({ wouldBlock: true, summary: GUARD_SUMMARY }, (id) =>
      id === FRESH_JOB_ID ? guardBlockedJobResult(id, 41) : guardBlockedJobResult(id, 40),
    );
    queueSyncJobs(FRESH_JOB_ID, LATER_JOB_ID);

    renderParityPage();
    const override = await openOverride();

    expect(within(override).getByText(/If the guard no longer blocks it, it runs as a normal sync\./)).toBeInTheDocument();
    expect(await within(override).findByText(/removed=41 \(threshold-relevant\)/)).toBeInTheDocument();
    expect(within(override).queryByText(GUARD_SUMMARY)).not.toBeInTheDocument();
    expect(syncConfirms()).toEqual([false]);
    const submit = within(override).getByRole("button", { name: "Sync anyway" });
    expect(submit).toBeDisabled();

    fireEvent.change(within(override).getByRole("textbox"), { target: { value: "SYNC ANYWAY" } });
    await waitFor(() => expect(submit).toBeEnabled());
    expect(syncConfirms()).toEqual([false]);

    fireEvent.click(submit);
    await waitFor(() => expect(syncConfirms()).toEqual([false, true]));
  });

  it("does not let an old refusal reach confirm true: the override asks the guard afresh", async () => {
    // The first Sync now was refused with removed=600. By the time the user
    // opens Sync anyway the engine has not answered the fresh request yet.
    mockParityApi({ wouldBlock: false }, (id) =>
      id === JOB_ID
        ? guardBlockedJobResult(id, 600)
        : { data: job(id, "running"), response: { ok: true } },
    );
    queueSyncJobs(JOB_ID, FRESH_JOB_ID);

    renderParityPage();
    await syncNow();
    expect(await screen.findByText(/removed=600 \(threshold-relevant\)/)).toBeInTheDocument();

    const override = await openOverride();
    await waitFor(() => expect(syncConfirms()).toEqual([false, false]));

    expect(within(override).queryByText(/removed=600/)).not.toBeInTheDocument();
    expect(within(override).queryByRole("textbox")).not.toBeInTheDocument();
    expect(within(override).queryByRole("button", { name: "Sync anyway" })).not.toBeInTheDocument();
    expect(syncConfirms()).not.toContain(true);
  });

  it("discards a refusal when the overlay is closed and opened again", async () => {
    mockParityApi({ wouldBlock: true, summary: GUARD_SUMMARY }, (id) =>
      id === FRESH_JOB_ID ? guardBlockedJobResult(id, 41) : { data: job(id, "running"), response: { ok: true } },
    );
    queueSyncJobs(FRESH_JOB_ID, LATER_JOB_ID);

    renderParityPage();
    const first = await openOverride();
    fireEvent.change(await within(first).findByRole("textbox"), { target: { value: "SYNC ANYWAY" } });
    await closeOverride(first);

    const second = await openOverride();
    await waitFor(() => expect(syncConfirms()).toEqual([false, false]));

    expect(within(second).queryByText(/removed=41/)).not.toBeInTheDocument();
    expect(within(second).queryByRole("textbox")).not.toBeInTheDocument();
    expect(within(second).queryByRole("button", { name: "Sync anyway" })).not.toBeInTheDocument();
    expect(syncConfirms()).not.toContain(true);
  });

  it("runs the fresh sync as a normal one and offers no override when the guard no longer blocks", async () => {
    mockParityApi({ wouldBlock: true, summary: GUARD_SUMMARY }, (id) => ({
      data: succeededJob(id),
      response: { ok: true },
    }));
    queueSyncJobs(FRESH_JOB_ID);

    renderParityPage();
    const override = await openOverride();

    expect(
      await within(override).findByText("The guard no longer blocks this sync, so it ran as a normal sync."),
    ).toBeInTheDocument();
    expect(within(override).queryByRole("textbox")).not.toBeInTheDocument();
    expect(within(override).queryByRole("button", { name: "Sync anyway" })).not.toBeInTheDocument();
    expect(await screen.findByText("Sync finished")).toBeInTheDocument();
    expect(syncConfirms()).toEqual([false]);
  });

  it("keeps the overlay open, shows the error and drops the refusal when the override request fails", async () => {
    mockParityApi({ wouldBlock: true, summary: GUARD_SUMMARY }, (id) => guardBlockedJobResult(id, 41));
    queueSyncJobs(FRESH_JOB_ID);

    renderParityPage();
    const override = await openOverride();
    const box = await within(override).findByRole("textbox");

    mockPost.mockResolvedValueOnce({
      error: { code: "on_battery", message: "Parity writes are held while the server is on battery." },
      response: { ok: false },
    });
    fireEvent.change(box, { target: { value: "SYNC ANYWAY" } });
    fireEvent.click(within(override).getByRole("button", { name: "Sync anyway" }));

    expect(await within(override).findByText("Parity writes are held while the server is on battery.")).toBeInTheDocument();
    expect(screen.queryByText("Sync started")).not.toBeInTheDocument();
    expect(syncConfirms()).toEqual([false, true]);
    expect(within(override).queryByRole("textbox")).not.toBeInTheDocument();
    expect(within(override).queryByRole("button", { name: "Sync anyway" })).not.toBeInTheDocument();
  });

  it("offers no override while the guard reads clear and nothing was refused", async () => {
    mockParityApi({ wouldBlock: false }, (id) => ({ data: succeededJob(id), response: { ok: true } }));
    queueSyncJobs(JOB_ID);

    renderParityPage();
    const dialog = await openSyncDialog();

    expect(within(dialog).queryByRole("button", { name: "Sync anyway" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sync anyway…" })).not.toBeInTheDocument();
  });
});

function guardBlockedJobResult(id: string, removed = 40): unknown {
  return { data: guardBlockedJob(id, removed), response: { ok: true } };
}
