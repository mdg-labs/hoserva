import { Activity } from "lucide-react";
import { useTranslation } from "react-i18next";

import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { EmptyState } from "@/components/patterns/empty-state";
import { InlineNote } from "@/components/patterns/inline-note";

interface WakeEventRow {
  device: string;
  timestamp: string;
  wakeCount: number;
}

const columns = (t: ReturnType<typeof useTranslation>["t"]): DataTableColumn<WakeEventRow>[] => [
  { id: "device", header: t("wakeEvents.columns.device"), cell: (row) => row.device },
  { id: "timestamp", header: t("wakeEvents.columns.timestamp"), cell: (row) => row.timestamp },
  { id: "count", header: t("wakeEvents.columns.count"), cell: (row) => row.wakeCount },
];

export function WakeEventsPage(): React.ReactElement {
  const { t } = useTranslation();
  const rows: WakeEventRow[] = [];

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-2xl font-semibold font-heading">{t("storageNav.wakeEvents")}</h1>
        <p className="text-muted-foreground">{t("wakeEvents.description")}</p>
      </div>
      <InlineNote description={t("wakeEvents.phase1Note")} />
      {rows.length === 0 ? (
        <EmptyState
          icon={Activity}
          title={t("wakeEvents.empty.title")}
          description={t("wakeEvents.empty.description")}
        />
      ) : (
        <DataTable columns={columns(t)} rows={rows} getRowKey={(row) => `${row.device}-${row.timestamp}`} />
      )}
    </div>
  );
}
