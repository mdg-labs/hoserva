import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { AppShell } from "@/components/patterns/app-shell";

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

const missingDisk = {
  device: "/dev/sdx",
  mountPoint: "/mnt/disk4",
  role: "data",
  state: "missing",
};

// mockDegradedSystemData wires /status to acknowledged (a closure flag this
// file's own tests flip): arrayDegraded stays true for as long as the disk
// is still missing — production never clears it on acknowledgement alone
// (#385 finding 2) — and arrayDegradedAcknowledged is what a test flips
// instead, so a test can drive the same request sequence a real
// acknowledge-then-refresh does. released (default: the same closure as
// acknowledged, matching a real successful acknowledge where both flip
// together) is storageServicesReleased — the field #385 finding 2 added so
// a test can also drive the case where the acknowledgement stands
// (arrayDegradedAcknowledged) but the transition it triggered did not
// actually start anything.
function mockDegradedSystemData(acknowledged: () => boolean, released: () => boolean = acknowledged): void {
  mockGet.mockImplementation((path: string) => {
    if (path === "/status") {
      return Promise.resolve({
        data: {
          healthy: false,
          summary: "Degraded",
          arrayDegraded: true,
          arrayDegradedAcknowledged: acknowledged(),
          storageServicesReleased: released(),
          activeJobs: 0,
        },
        response: { ok: true },
      });
    }
    if (path === "/pool") {
      return Promise.resolve({
        data: { mounted: true, disks: [missingDisk] },
        response: { ok: true },
      });
    }
    if (path === "/jobs") {
      return Promise.resolve({ data: { jobs: [] }, response: { ok: true } });
    }
    if (path === "/doctor") {
      return Promise.resolve({ data: { overall: "pass", checks: [] }, response: { ok: true } });
    }
    if (path === "/notifications") {
      return Promise.resolve({ data: { groups: [], unreadCount: 0 }, response: { ok: true } });
    }
    return Promise.resolve({ data: null, response: { ok: false } });
  });
}

describe("PersistentBanners degraded acknowledgement", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
    mockMatchMedia();
  });

  it("names the missing disk in the confirmation and swaps to the acknowledged banner on success", async () => {
    let acknowledged = false;
    mockDegradedSystemData(() => acknowledged);
    mockPost.mockImplementation((path: string) => {
      if (path === "/array/degraded/acknowledge") {
        acknowledged = true;
        return Promise.resolve({
          data: {
            healthy: true,
            summary: "All checks passed",
            arrayDegraded: true,
            arrayDegradedAcknowledged: true,
            storageServicesReleased: true,
          },
          response: { ok: true },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <AppShell>
          <div>Page body</div>
        </AppShell>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Array degraded")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Acknowledge and start services" }));

    expect(await screen.findByText("Acknowledge the degraded array?")).toBeInTheDocument();
    expect(screen.getByText("/mnt/disk4 (/dev/sdx)")).toBeInTheDocument();

    // The banner's own action button is inert (aria-hidden) behind the
    // open modal dialog, so only the dialog's own confirm button matches
    // here.
    fireEvent.click(screen.getAllByRole("button", { name: "Acknowledge and start services" })[0]);

    await waitFor(() => {
      expect(mockPost).toHaveBeenCalledWith("/array/degraded/acknowledge");
    });
    // The array is still degraded (a disk is still physically missing —
    // #385 finding 2), so the banner never disappears outright: it swaps
    // to the acknowledged title and drops the acknowledge action, since
    // there is nothing left to acknowledge.
    await waitFor(() => {
      expect(screen.getByText("Array degraded — running acknowledged")).toBeInTheDocument();
    });
    expect(
      screen.queryByRole("button", { name: "Acknowledge and start services" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText("Acknowledge the degraded array?"),
    ).not.toBeInTheDocument();
  });

  it("keeps the confirmation open and shows the error on refusal, never a silent success", async () => {
    mockDegradedSystemData(() => false);
    mockPost.mockImplementation((path: string) => {
      if (path === "/array/degraded/acknowledge") {
        return Promise.resolve({
          error: { code: "array_not_degraded", message: "the array is not degraded" },
          response: { ok: false },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <AppShell>
          <div>Page body</div>
        </AppShell>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Array degraded")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Acknowledge and start services" }));
    expect(await screen.findByText("Acknowledge the degraded array?")).toBeInTheDocument();

    // The banner's own action button is inert (aria-hidden) behind the
    // open modal dialog, so only the dialog's own confirm button matches
    // here.
    fireEvent.click(screen.getAllByRole("button", { name: "Acknowledge and start services" })[0]);

    expect(await screen.findByText("the array is not degraded")).toBeInTheDocument();
    // The dialog stays open with the confirmation text still in place, so
    // the operator can retry — an error must never replace it or land
    // behind a closed dialog.
    expect(screen.getByText("Acknowledge the degraded array?")).toBeInTheDocument();
    expect(screen.getByText("/mnt/disk4 (/dev/sdx)")).toBeInTheDocument();
  });

  it("ignores a cancel attempt while the request is still pending", async () => {
    mockDegradedSystemData(() => false);
    let resolvePost: (value: unknown) => void = () => {};
    mockPost.mockImplementation((path: string) => {
      if (path === "/array/degraded/acknowledge") {
        return new Promise((resolve) => {
          resolvePost = resolve;
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <AppShell>
          <div>Page body</div>
        </AppShell>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Array degraded")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Acknowledge and start services" }));
    expect(await screen.findByText("Acknowledge the degraded array?")).toBeInTheDocument();

    fireEvent.click(screen.getAllByRole("button", { name: "Acknowledge and start services" })[0]);

    // The request is now in flight (resolvePost has not been called yet).
    // Escape and a backdrop click both come through the dialog's own
    // onOpenChange(false) the same way this Cancel click does (#385
    // finding 6) — none of them may close the dialog while pending, or a
    // later refusal or failure would land on a surface the user already
    // dismissed.
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByText("Acknowledge the degraded array?")).toBeInTheDocument();

    resolvePost({
      error: { code: "array_not_degraded", message: "the array is not degraded" },
      response: { ok: false },
    });
    expect(await screen.findByText("the array is not degraded")).toBeInTheDocument();
  });

  it("clears a stale error from an earlier attempt when the dialog is reopened", async () => {
    mockDegradedSystemData(() => false);
    mockPost.mockImplementation((path: string) => {
      if (path === "/array/degraded/acknowledge") {
        return Promise.resolve({
          error: { code: "array_not_degraded", message: "the array is not degraded" },
          response: { ok: false },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    render(
      <MemoryRouter>
        <AppShell>
          <div>Page body</div>
        </AppShell>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Array degraded")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Acknowledge and start services" }));
    fireEvent.click(screen.getAllByRole("button", { name: "Acknowledge and start services" })[0]);
    expect(await screen.findByText("the array is not degraded")).toBeInTheDocument();

    // Not pending anymore (the mutation already settled with an error), so
    // this close is allowed — then reopening must not show the previous
    // failure again until a new attempt actually produces one.
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => {
      expect(screen.queryByText("Acknowledge the degraded array?")).not.toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole("button", { name: "Acknowledge and start services" }));
    expect(await screen.findByText("Acknowledge the degraded array?")).toBeInTheDocument();
    expect(screen.queryByText("the array is not degraded")).not.toBeInTheDocument();
  });

  // #385 finding 2: a 409 array_services_not_started leaves
  // disk.StorageGate itself acknowledged (production's own Ready() flips
  // the moment Acknowledge() succeeds, before the failing transition ever
  // runs), so a later /status poll reports arrayDegradedAcknowledged: true
  // while storageServicesReleased stays false. The banner must never read
  // arrayDegradedAcknowledged alone as "services are running".
  it("never claims the services are running, and keeps the retry action, after a 409 array_services_not_started even once a later poll reports the acknowledgement", async () => {
    let acknowledgedButNotReleased = false;
    mockDegradedSystemData(
      () => acknowledgedButNotReleased,
      () => false,
    );
    mockPost.mockImplementation((path: string) => {
      if (path === "/array/degraded/acknowledge") {
        acknowledgedButNotReleased = true;
        return Promise.resolve({
          error: {
            code: "array_services_not_started",
            message: "the array was acknowledged, but the gated services could not be started",
          },
          response: { ok: false },
        });
      }
      return Promise.resolve({ data: null, response: { ok: false } });
    });

    // useSystemData polls every 30s (POLL_MS) through window.setInterval;
    // this test drives that poll deterministically instead of waiting on
    // it or faking every timer in the page (which would also break
    // testing-library's own waitFor polling) — only the 30s interval
    // itself is intercepted, and its callback captured to invoke by hand.
    const intervalCallbacks: Array<() => void> = [];
    const realSetInterval = window.setInterval.bind(window);
    const setIntervalSpy = vi
      .spyOn(window, "setInterval")
      .mockImplementation(((handler: TimerHandler, timeout?: number, ...args: unknown[]) => {
        if (timeout === 30_000) {
          intervalCallbacks.push(handler as () => void);
          return 0 as unknown as ReturnType<typeof window.setInterval>;
        }
        return realSetInterval(handler as TimerHandler, timeout, ...args);
      }) as typeof window.setInterval);

    render(
      <MemoryRouter>
        <AppShell>
          <div>Page body</div>
        </AppShell>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Array degraded")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Acknowledge and start services" }));
    expect(await screen.findByText("Acknowledge the degraded array?")).toBeInTheDocument();
    fireEvent.click(screen.getAllByRole("button", { name: "Acknowledge and start services" })[0]);

    expect(
      await screen.findByText(
        "the array was acknowledged, but the gated services could not be started",
      ),
    ).toBeInTheDocument();

    // Settled with an error, not pending — the dialog may be closed, the
    // same as the earlier "clears a stale error" case above.
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => {
      expect(screen.queryByText("Acknowledge the degraded array?")).not.toBeInTheDocument();
    });

    intervalCallbacks.forEach((cb) => cb());

    await waitFor(() => {
      expect(
        screen.queryByText("Array degraded — running acknowledged"),
      ).not.toBeInTheDocument();
    });
    expect(await screen.findByText("Array degraded")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Acknowledge and start services" }),
    ).toBeInTheDocument();

    setIntervalSpy.mockRestore();
  });
});
