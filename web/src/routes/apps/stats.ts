import type { components } from "@/lib/api/client";

export type StatsSample = components["schemas"]["AppStats"];
export type CounterField = "networkRxBytes" | "networkTxBytes" | "blockReadBytes" | "blockWriteBytes";
export type RatePoint = { at: string; bytesPerSecond: number };

export const STATS_POLL_MS = 3000;
// Five minutes of samples at the polling interval.
export const MAX_SAMPLES = 100;

export function appendSample(samples: StatsSample[], sample: StatsSample): StatsSample[] {
  return [...samples, sample].slice(-MAX_SAMPLES);
}

// The Engine reports running totals, so a rate is the growth between two
// consecutive samples over the time between them. A total that went down
// (the container restarted) or samples with no time between them give no
// rate, never a negative or infinite one.
export function rateSeries(samples: StatsSample[], field: CounterField): RatePoint[] {
  const points: RatePoint[] = [];
  for (let index = 1; index < samples.length; index += 1) {
    const previous = samples[index - 1];
    const current = samples[index];
    const seconds = (Date.parse(current.at) - Date.parse(previous.at)) / 1000;
    const growth = current[field] - previous[field];
    if (seconds > 0 && growth >= 0) {
      points.push({ at: current.at, bytesPerSecond: growth / seconds });
    }
  }
  return points;
}
