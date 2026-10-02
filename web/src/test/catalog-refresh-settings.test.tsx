import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { SchedulesSettingsPage } from "@/routes/settings/schedules";

const mockGet = vi.fn();
const mockPut = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: {
    GET: (...args: unknown[]) => mockGet(...args),
    PUT: (...args: unknown[]) => mockPut(...args),
  },
}));

const SCHEDULES = {
  chain: {
    startTime: "02:00",
    weeklyScrubDay: 0,
    schedulePreview: "every day at 02:00",
    nextRun: "2026-06-16T00:00:00.000Z",
    steps: [{ id: "mover", enabled: true }],
  },
  otherJobs: [],
  conflicts: [],
};

function ok<T>(data: T) {
  return Promise.resolve({ data, response: { ok: true } });
}

function installGet(catalog: () => Promise<unknown>, schedules: () => Promise<unknown> = () => ok(SCHEDULES)): void {
  mockGet.mockImplementation((path: string) => {
    if (path === "/settings/catalog") {
      return catalog();
    }
    if (path === "/settings/schedules") {
      return schedules();
    }
    return Promise.resolve({ error: { code: "not_found", message: `unexpected ${path}` }, response: { ok: false } });
  });
}

function renderPage(): ReturnType<typeof render> {
  return render(
    <MemoryRouter>
      <SchedulesSettingsPage />
    </MemoryRouter>,
  );
}

async function card(): Promise<HTMLElement> {
  const title = await screen.findByText("Catalog refresh");
  const found = title.closest("[data-slot=card]");
  if (!(found instanceof HTMLElement)) {
    throw new Error("no catalog refresh card");
  }
  return found;
}

describe("Catalog refresh card on the schedules page", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPut.mockReset();
  });

  it("shows the saved interval and check-on-open", async () => {
    installGet(() => ok({ refreshInterval: "12h", checkOnOpen: false }));
    renderPage();
    const el = await card();

    expect(await within(el).findByText("Every 12 hours")).toBeInTheDocument();
    expect(within(el).getByRole("switch", { name: "Check when the catalog is opened" })).not.toBeChecked();
  });

  it("saves a new interval and shows it once the daemon returns it", async () => {
    let saved = { refreshInterval: "24h", checkOnOpen: true };
    installGet(() => ok(saved));
    mockPut.mockImplementation((_path: string, init: { body: Record<string, string> }) => {
      saved = { ...saved, ...init.body } as typeof saved;
      return ok(saved);
    });
    renderPage();
    const el = await card();

    fireEvent.click(await within(el).findByRole("combobox", { name: "Check in the background" }));
    const option = await screen.findByRole("option", { name: "Every 6 hours" });
    fireEvent.pointerDown(option, { pointerType: "mouse" });
    fireEvent.pointerUp(option, { pointerType: "mouse" });
    fireEvent.click(option);

    await waitFor(() =>
      expect(mockPut).toHaveBeenCalledWith("/settings/catalog", { body: { refreshInterval: "6h" } }),
    );
    expect(await within(el).findByText("Every 6 hours")).toBeInTheDocument();
  });

  it("saves check-on-open on its own, leaving the interval out", async () => {
    let saved = { refreshInterval: "24h", checkOnOpen: true };
    installGet(() => ok(saved));
    mockPut.mockImplementation((_path: string, init: { body: Record<string, boolean> }) => {
      saved = { ...saved, ...init.body };
      return ok(saved);
    });
    renderPage();
    const el = await card();

    fireEvent.click(await within(el).findByRole("switch", { name: "Check when the catalog is opened" }));

    await waitFor(() => expect(mockPut).toHaveBeenCalledWith("/settings/catalog", { body: { checkOnOpen: false } }));
    await waitFor(() =>
      expect(within(el).getByRole("switch", { name: "Check when the catalog is opened" })).not.toBeChecked(),
    );
  });

  it("shows a refused save as an error and keeps showing the stored value", async () => {
    installGet(() => ok({ refreshInterval: "24h", checkOnOpen: true }));
    mockPut.mockResolvedValue({
      error: { code: "invalid_catalog_interval", message: "the interval is not allowed" },
      response: { ok: false },
    });
    renderPage();
    const el = await card();

    fireEvent.click(await within(el).findByRole("switch", { name: "Check when the catalog is opened" }));

    expect(await within(el).findByText("the interval is not allowed")).toBeInTheDocument();
    expect(within(el).getByText("Could not save the catalog refresh settings.")).toBeInTheDocument();
    expect(within(el).getByRole("switch", { name: "Check when the catalog is opened" })).toBeChecked();
  });

  it("shows a rejected save as an error too", async () => {
    installGet(() => ok({ refreshInterval: "24h", checkOnOpen: true }));
    mockPut.mockRejectedValue(new Error("network down"));
    renderPage();
    const el = await card();

    fireEvent.click(await within(el).findByRole("switch", { name: "Check when the catalog is opened" }));

    expect(await within(el).findByText("network down")).toBeInTheDocument();
    expect(within(el).getByRole("switch", { name: "Check when the catalog is opened" })).toBeChecked();
  });

  it("shows a failed read as an error with no controls, then loads on retry", async () => {
    let calls = 0;
    installGet(() => {
      calls += 1;
      return calls === 1
        ? Promise.resolve({ error: { code: "internal", message: "settings unavailable" }, response: { ok: false } })
        : ok({ refreshInterval: "1h", checkOnOpen: true });
    });
    renderPage();
    const el = await card();

    expect(await within(el).findByText("settings unavailable")).toBeInTheDocument();
    expect(within(el).queryByRole("switch")).not.toBeInTheDocument();

    fireEvent.click(within(el).getByRole("button", { name: "Try again" }));
    expect(await within(el).findByText("Every hour")).toBeInTheDocument();
    expect(within(el).queryByText("settings unavailable")).not.toBeInTheDocument();
  });

  it("is still offered when the schedules themselves fail to load", async () => {
    installGet(
      () => ok({ refreshInterval: "24h", checkOnOpen: true }),
      () => Promise.resolve({ error: { code: "internal", message: "schedules unavailable" }, response: { ok: false } }),
    );
    renderPage();

    expect(await screen.findByText("schedules unavailable")).toBeInTheDocument();
    const el = await card();
    expect(await within(el).findByText("Daily")).toBeInTheDocument();
  });
});
