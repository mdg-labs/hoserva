import { Gauge } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { TimeSeriesChart, type ChartPoint } from "@/components/patterns/chart";
import { EmptyState } from "@/components/patterns/empty-state";
import { LoadingBlock } from "@/components/patterns/loading";
import { getAppStats } from "@/lib/api/operations";
import { apiErrorMessage, isAbortError } from "@/lib/api/request";
import type { App } from "@/routes/apps/containers";
import {
  appendSample,
  rateSeries,
  STATS_POLL_MS,
  type CounterField,
  type StatsSample,
} from "@/routes/apps/stats";
import { formatBytes } from "@/routes/storage-setup/config-preview";

const NOT_RUNNING_CODE = "app_not_running";
const RATE_CHARTS = [
  { field: "networkRxBytes", name: "networkRx" },
  { field: "networkTxBytes", name: "networkTx" },
  { field: "blockReadBytes", name: "diskRead" },
  { field: "blockWriteBytes", name: "diskWrite" },
] as const;
const KIB = 1024;
const MIB = 1024 * 1024;

type StatsState = {
  id: string;
  samples: StatsSample[];
  stopped: boolean;
  failure: string | null;
  loaded: boolean;
};

function initialState(id: string): StatsState {
  return { id, samples: [], stopped: false, failure: null, loaded: false };
}

// Samples the container once per interval while the tab is mounted and the
// browser page is visible, one request at a time, and keeps only the most
// recent ones in memory.
function useAppStats(id: string): StatsState {
  const [state, setState] = useState<StatsState>(() => initialState(id));

  useEffect(() => {
    const controller = new AbortController();
    const { signal } = controller;
    let timer: number | undefined;
    const update = (change: (current: StatsState) => StatsState): void => {
      if (!signal.aborted) {
        setState((current) => change(current.id === id ? current : initialState(id)));
      }
    };

    const sample = async (): Promise<void> => {
      if (!document.hidden) {
        try {
          const result = await getAppStats(id, signal);
          if (result.error?.code === NOT_RUNNING_CODE) {
            update(() => ({ id, samples: [], stopped: true, failure: null, loaded: true }));
          } else if (result.error !== undefined || result.response?.ok === false || result.data === undefined) {
            const failure = apiErrorMessage(result.error);
            update((current) => ({ ...current, stopped: false, failure, loaded: true }));
          } else {
            const next = result.data;
            update((current) => ({
              id,
              samples: appendSample(current.samples, next),
              stopped: false,
              failure: null,
              loaded: true,
            }));
          }
        } catch (err: unknown) {
          if (isAbortError(err, signal)) {
            return;
          }
          const failure = apiErrorMessage(undefined, err instanceof Error ? err.message : undefined);
          update((current) => ({ ...current, stopped: false, failure, loaded: true }));
        }
      }
      if (!signal.aborted) {
        timer = window.setTimeout(() => void sample(), STATS_POLL_MS);
      }
    };
    void sample();

    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [id]);

  return state.id === id ? state : initialState(id);
}

function timeLabel(at: string): string {
  return new Date(at).toLocaleTimeString();
}

export function StatsTab({ app }: { app: App }): React.ReactElement {
  const { t } = useTranslation();
  const stats = useAppStats(app.id);
  const { samples } = stats;
  const latest = samples.length > 0 ? samples[samples.length - 1] : null;

  if (stats.stopped) {
    return (
      <EmptyState
        icon={Gauge}
        title={t("apps.stats.stopped.title")}
        description={t("apps.stats.stopped.description")}
      />
    );
  }
  if (!stats.loaded) {
    return <LoadingBlock />;
  }

  const cpu: ChartPoint[] = samples.map((sample) => ({ label: timeLabel(sample.at), value: sample.cpuPercent }));
  const memory: ChartPoint[] = samples.map((sample) => ({
    label: timeLabel(sample.at),
    value: sample.memoryBytes / MIB,
  }));
  const rates = (field: CounterField): ChartPoint[] =>
    rateSeries(samples, field).map((point) => ({
      label: timeLabel(point.at),
      value: point.bytesPerSecond / KIB,
    }));
  const memoryDescription =
    latest === null
      ? undefined
      : latest.memoryLimitBytes > 0
        ? t("apps.stats.memory.ofLimit", {
            used: formatBytes(latest.memoryBytes),
            limit: formatBytes(latest.memoryLimitBytes),
          })
        : t("apps.stats.memory.noLimit", { used: formatBytes(latest.memoryBytes) });
  const waiting = {
    emptyTitle: t("apps.stats.waiting.title"),
    emptyDescription: t("apps.stats.waiting.description"),
  };

  if (stats.failure !== null && samples.length === 0) {
    return <Banner tone="error" title={t("apps.stats.loadFailed")} description={stats.failure} />;
  }

  return (
    <div className="flex flex-col gap-4">
      {stats.failure !== null ? (
        <Banner tone="error" title={t("apps.stats.loadFailed")} description={stats.failure} />
      ) : null}
      <div className="grid gap-4 lg:grid-cols-2">
        <TimeSeriesChart
          title={t("apps.stats.cpu.title")}
          description={t("apps.stats.cpu.description")}
          data={cpu}
          valueFormatter={(value) => t("apps.stats.cpu.value", { value: value.toFixed(1) })}
          {...waiting}
        />
        <TimeSeriesChart
          title={t("apps.stats.memory.title")}
          description={memoryDescription}
          data={memory}
          valueFormatter={(value) => t("apps.stats.memory.value", { value: value.toFixed(1) })}
          {...waiting}
        />
        {RATE_CHARTS.map(({ field, name }) => (
          <TimeSeriesChart
            key={field}
            title={t(`apps.stats.${name}.title`)}
            data={rates(field)}
            valueFormatter={(value) => t("apps.stats.rate", { value: value.toFixed(1) })}
            {...waiting}
          />
        ))}
      </div>
    </div>
  );
}
