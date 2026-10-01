import { describe, expect, it } from "vitest";

import { appendSample, MAX_SAMPLES, rateSeries, type StatsSample } from "@/routes/apps/stats";

function sample(secondsIn: number, rx: number, extra: Partial<StatsSample> = {}): StatsSample {
  return {
    at: new Date(Date.UTC(2026, 9, 1, 12, 0, secondsIn)).toISOString(),
    cpuPercent: 0,
    memoryBytes: 0,
    memoryLimitBytes: 0,
    networkRxBytes: rx,
    networkTxBytes: 0,
    blockReadBytes: 0,
    blockWriteBytes: 0,
    ...extra,
  };
}

describe("rateSeries", () => {
  it("is the growth of a total between consecutive samples over the time between them", () => {
    const points = rateSeries([sample(0, 1000), sample(2, 5000), sample(7, 5500)], "networkRxBytes");
    expect(points.map((point) => point.bytesPerSecond)).toEqual([2000, 100]);
  });

  it("has no rate before a second sample", () => {
    expect(rateSeries([sample(0, 1000)], "networkRxBytes")).toEqual([]);
  });

  it("skips a total that went down, as after a restart, and samples at the same time", () => {
    const points = rateSeries(
      [sample(0, 9000), sample(3, 100), sample(6, 400), sample(6, 900)],
      "networkRxBytes",
    );
    expect(points.map((point) => point.bytesPerSecond)).toEqual([100]);
  });
});

describe("appendSample", () => {
  it("keeps only the most recent samples", () => {
    let samples: StatsSample[] = [];
    for (let index = 0; index < MAX_SAMPLES + 5; index += 1) {
      samples = appendSample(samples, sample(index, index));
    }
    expect(samples).toHaveLength(MAX_SAMPLES);
    expect(samples[0].networkRxBytes).toBe(5);
  });
});
