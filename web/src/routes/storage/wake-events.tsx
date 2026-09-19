import { Activity } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { EmptyState } from "@/components/patterns/empty-state";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import { hoservaClient, type components } from "@/lib/api/client";

type WakeEventRow = {
  device: string;
  timestamp: string;
  wakeCount: number;
};

type SpinTransition = components["schemas"]["SpinTransition"];
type DailyWakeCount = components["schemas"]["DailyWakeCount"];

const columns = (t: ReturnType<typeof useTranslation>["t"]): DataTableColumn<WakeEventRow>[] => [
  { id: "device", header: t("wakeEvents.columns.device"), cell: (row) => row.device },
  { id: "timestamp", header: t("wakeEvents.columns.timestamp"), cell: (row) => row.timestamp },
  { id: "count", header: t("wakeEvents.columns.count"), cell: (row) => row.wakeCount },
];

function buildWakeEventRows(events: SpinTransition[], dailyWakeCounts: DailyWakeCount[]): WakeEventRow[] {
  const today = new Date().toISOString().slice(0, 10);
  const devices = new Set<string>();

  for (const event of events) {
    if (event.fromState === "standby" && event.toState === "active") {
      devices.add(event.device);
    }
  }
  for (const count of dailyWakeCounts) {
    devices.add(count.device);
  }

  const rows: WakeEventRow[] = [];
  for (const device of devices) {
    const wakes = events.filter((event) => event.device === device && event.fromState === "standby" && event.toState === "active");
    const lastWake = wakes.reduce<string | null>((latest, event) => {
      if (latest === null || event.at > latest) {
        return event.at;
      }
      return latest;
    }, null);
    const wakeCount = dailyWakeCounts.find((count) => count.device === device && count.date === today)?.count ?? 0;

    if (lastWake === null && wakeCount === 0) {
      continue;
    }

    rows.push({
      device,
      timestamp: lastWake ? new Date(lastWake).toLocaleString() : "—",
      wakeCount,
    });
  }

  return rows.sort((a, b) => a.device.localeCompare(b.device));
}

export function WakeEventsPage(): React.ReactElement {
  const { t } = useTranslation();
  const [rows, setRows] = useState<WakeEventRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    hoservaClient
      .GET("/disks/wake-events", { signal: controller.signal })
      .then((result) => {
        if (result.error) {
          setError(result.error.message);
          return;
        }
        const events = result.data?.events ?? [];
        const dailyWakeCounts = result.data?.dailyWakeCounts ?? [];
        setRows(buildWakeEventRows(events, dailyWakeCounts));
      })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) {
          setError(err instanceof Error ? err.message : String(err));
        }
      });
    return () => controller.abort();
  }, []);

  const tableRows = useMemo(() => rows ?? [], [rows]);

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-2xl font-semibold font-heading">{t("storageNav.wakeEvents")}</h1>
        <p className="text-muted-foreground">{t("wakeEvents.description")}</p>
      </div>
      <InlineNote description={t("wakeEvents.phase1Note")} />
      {error ? (
        <p className="text-destructive">{error}</p>
      ) : rows === null ? (
        <LoadingBlock />
      ) : tableRows.length === 0 ? (
        <EmptyState
          icon={Activity}
          title={t("wakeEvents.empty.title")}
          description={t("wakeEvents.empty.description")}
        />
      ) : (
        <DataTable columns={columns(t)} rows={tableRows} getRowKey={(row) => `${row.device}-${row.timestamp}`} />
      )}
    </div>
  );
}
