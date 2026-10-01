import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { App } from "@/routes/apps/containers";
import { StatsTab } from "@/routes/apps/stats-tab";

const mockGet = vi.fn();

vi.mock("@/lib/api/client", () => ({
  hoservaClient: { GET: (...args: unknown[]) => mockGet(...args) },
}));

vi.mock("@/components/patterns/chart", () => ({
  TimeSeriesChart: ({
    title,
    description,
    data,
  }: {
    title: string;
    description?: string;
    data: { value: number }[] | null;
  }) => (
    <section aria-label={title}>
      <p>{description}</p>
      <output>{JSON.stringify((data ?? []).map((point) => Number(point.value.toFixed(2))))}</output>
    </section>
  ),
}));

const app = { id: "id-web", name: "web", state: "running" } as App;
const START = Date.UTC(2026, 9, 1, 12, 0, 0);

function stats(secondsIn: number, extra: Record<string, number> = {}) {
  return {
    data: {
      at: new Date(START + secondsIn * 1000).toISOString(),
      cpuPercent: 10,
      memoryBytes: 512 * 1024 * 1024,
      memoryLimitBytes: 2048 * 1024 * 1024,
      networkRxBytes: 0,
      networkTxBytes: 0,
      blockReadBytes: 0,
      blockWriteBytes: 0,
      ...extra,
    },
    response: { ok: true },
  };
}

function series(title: string): string {
  const chart = screen.getByRole("region", { name: title });
  return chart.querySelector("output")?.textContent ?? "";
}

async function tick(ms: number): Promise<void> {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

beforeEach(() => {
  mockGet.mockReset();
  vi.useFakeTimers();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("Stats tab", () => {
  it("draws the samples taken so far, with rates from consecutive samples", async () => {
    const samples = [
      stats(0, { cpuPercent: 10 }),
      stats(3, { cpuPercent: 30, networkRxBytes: 6144, blockWriteBytes: 3 * 1024 * 10 }),
      stats(6, { cpuPercent: 20, networkRxBytes: 6144 + 3072, blockWriteBytes: 3 * 1024 * 10 }),
    ];
    mockGet.mockImplementation(() => Promise.resolve(samples.shift() ?? stats(9)));
    render(<StatsTab app={app} />);

    await tick(0);
    expect(mockGet.mock.calls[0][0]).toBe("/apps/{id}/stats");
    expect(mockGet.mock.calls[0][1].params.path.id).toBe("id-web");
    expect(series("Processor use")).toBe("[10]");
    expect(series("Network received (KiB/s)")).toBe("[]");
    expect(screen.getByText("512 MiB in use of 2.00 GiB allowed")).toBeInTheDocument();

    await tick(3000);
    await tick(3000);
    expect(series("Processor use")).toBe("[10,30,20]");
    expect(series("Network received (KiB/s)")).toBe("[2,1]");
    expect(series("Disk written (KiB/s)")).toBe("[10,0]");
    expect(series("Disk read (KiB/s)")).toBe("[0,0]");
  });

  it("samples every few seconds and stops when the tab closes", async () => {
    mockGet.mockImplementation(() => Promise.resolve(stats(0)));
    const view = render(<StatsTab app={app} />);

    await tick(0);
    expect(mockGet).toHaveBeenCalledTimes(1);
    await tick(2900);
    expect(mockGet).toHaveBeenCalledTimes(1);
    await tick(200);
    expect(mockGet).toHaveBeenCalledTimes(2);

    view.unmount();
    await tick(30_000);
    expect(mockGet).toHaveBeenCalledTimes(2);
  });

  it("does not sample while the page is hidden", async () => {
    mockGet.mockImplementation(() => Promise.resolve(stats(0)));
    Object.defineProperty(document, "hidden", { configurable: true, value: true });
    try {
      render(<StatsTab app={app} />);
      await tick(10_000);
      expect(mockGet).not.toHaveBeenCalled();
    } finally {
      Object.defineProperty(document, "hidden", { configurable: true, value: false });
    }
    await tick(3000);
    expect(mockGet).toHaveBeenCalledTimes(1);
  });

  it("explains that stats need a running app instead of drawing zeros", async () => {
    mockGet.mockImplementation(() =>
      Promise.resolve({ error: { code: "app_not_running", message: "web is not running" }, response: { ok: false } }),
    );
    render(<StatsTab app={app} />);

    await tick(0);
    expect(screen.getByText("Resource use needs a running app")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Processor use" })).not.toBeInTheDocument();
  });

  it("drops the old samples when the app stops, and starts a new series when it runs again", async () => {
    const answers = [
      stats(0, { cpuPercent: 50 }),
      { error: { code: "app_not_running", message: "web is not running" }, response: { ok: false } },
      stats(9, { cpuPercent: 5 }),
    ];
    mockGet.mockImplementation(() => Promise.resolve(answers.shift() ?? stats(12)));
    render(<StatsTab app={app} />);

    await tick(0);
    expect(series("Processor use")).toBe("[50]");
    await tick(3000);
    expect(screen.getByText("Resource use needs a running app")).toBeInTheDocument();
    await tick(3000);
    expect(series("Processor use")).toBe("[5]");
  });

  it("shows a failed first sample as an error, not as an empty chart", async () => {
    mockGet.mockImplementation(() =>
      Promise.resolve({ error: { code: "internal", message: "stats are down" }, response: { ok: false } }),
    );
    render(<StatsTab app={app} />);

    await tick(0);
    expect(screen.getByText("Could not read this app's resource use.")).toBeInTheDocument();
    expect(screen.getByText("stats are down")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Processor use" })).not.toBeInTheDocument();
  });

  it("keeps the samples it has when a later sample fails, and says so", async () => {
    const answers = [
      stats(0, { cpuPercent: 40 }),
      { error: { code: "internal", message: "stats are down" }, response: { ok: false } },
    ];
    mockGet.mockImplementation(() => Promise.resolve(answers.shift() ?? stats(6, { cpuPercent: 60 })));
    render(<StatsTab app={app} />);

    await tick(0);
    await tick(3000);
    expect(screen.getByText("stats are down")).toBeInTheDocument();
    expect(series("Processor use")).toBe("[40]");

    await tick(3000);
    expect(screen.queryByText("stats are down")).not.toBeInTheDocument();
    expect(series("Processor use")).toBe("[40,60]");
  });
});
