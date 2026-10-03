import type { TFunction } from "i18next";

import type { DataTableColumn } from "@/components/patterns/data-table";
import { StatusBadge } from "@/components/patterns/status-badge";
import { statusTone, type CheckStatus, type ReportRow } from "@/routes/tools-migrate/report";

export function checkLabel(t: TFunction, check: string): string {
  return t(`toolsMigrate.checks.${check}`, { defaultValue: check });
}

export function rowStatusBadge(t: TFunction, status: CheckStatus): React.ReactElement {
  return <StatusBadge tone={statusTone(status)}>{t(`toolsMigrate.status.${status}`, { defaultValue: status })}</StatusBadge>;
}

export function reportRowColumns(t: TFunction): DataTableColumn<ReportRow>[] {
  return [
    {
      id: "subject",
      header: t("toolsMigrate.columns.subject"),
      cell: (row) => <span className="font-medium">{row.subject ?? t("toolsMigrate.wholeSystem")}</span>,
    },
    { id: "check", header: t("toolsMigrate.columns.check"), cell: (row) => checkLabel(t, row.check) },
    { id: "status", header: t("toolsMigrate.columns.status"), cell: (row) => rowStatusBadge(t, row.status) },
    { id: "detail", header: t("toolsMigrate.columns.detail"), cell: (row) => row.detail, className: "whitespace-normal" },
  ];
}
