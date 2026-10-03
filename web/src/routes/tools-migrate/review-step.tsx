import { HardDrive } from "lucide-react";
import { useTranslation } from "react-i18next";

import { DataTable } from "@/components/patterns/data-table";
import { EmptyState } from "@/components/patterns/empty-state";
import { Button } from "@/components/ui/button";
import { listMigrationTemplates } from "@/lib/api/operations";
import { useApiQuery } from "@/lib/api/use-api-query";
import { CaptureWarningBanner } from "@/routes/tools-migrate/banners";
import {
  BOOT_CHECKS,
  DISK_CHECKS,
  SHARE_CHECKS,
  allTemplatesUnknown,
  captureWarning,
  hasMatchedDisks,
  reportRowKey,
  rowsOfChecks,
  type ReportRow,
} from "@/routes/tools-migrate/report";
import { reportRowColumns } from "@/routes/tools-migrate/columns";
import { TemplatesPreview } from "@/routes/tools-migrate/templates";

function RowSection({
  title,
  description,
  rows,
}: {
  title: string;
  description: string;
  rows: ReportRow[];
}): React.ReactElement {
  const { t } = useTranslation();

  return (
    <section className="flex flex-col gap-2" aria-label={title}>
      <h3 className="font-medium">{title}</h3>
      <p className="text-muted-foreground text-sm">{description}</p>
      {rows.length > 0 ? (
        <DataTable rows={rows} getRowKey={reportRowKey} columns={reportRowColumns(t)} />
      ) : (
        <p className="text-muted-foreground text-sm">{t("toolsMigrate.review.noRows")}</p>
      )}
    </section>
  );
}

export function ReviewStep({
  rows,
  onScanAgain,
}: {
  rows: ReportRow[];
  onScanAgain: () => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const matched = hasMatchedDisks(rows);
  const templates = useApiQuery({
    queryKey: "migration-templates",
    queryFn: (signal) => listMigrationTemplates(signal),
    fallbackError: t("toolsMigrate.templates.loadFailed"),
  });
  const capture = captureWarning(rows);

  return (
    <div className="flex flex-col gap-6">
      {capture ? (
        <CaptureWarningBanner
          detail={capture.detail}
          allUnknown={templates.data !== null && allTemplatesUnknown(templates.data)}
        />
      ) : null}

      {matched ? null : (
        <EmptyState
          icon={HardDrive}
          title={t("toolsMigrate.review.noMatchedDisks.title")}
          description={t("toolsMigrate.review.noMatchedDisks.description")}
          action={
            <Button variant="outline" onClick={onScanAgain}>
              {t("toolsMigrate.actions.scanAgain")}
            </Button>
          }
        />
      )}

      <RowSection
        title={t("toolsMigrate.review.disks.title")}
        description={t("toolsMigrate.review.disks.description")}
        rows={rowsOfChecks(rows, DISK_CHECKS)}
      />
      <RowSection
        title={t("toolsMigrate.review.boot.title")}
        description={t("toolsMigrate.review.boot.description")}
        rows={rowsOfChecks(rows, BOOT_CHECKS)}
      />
      <RowSection
        title={t("toolsMigrate.review.shares.title")}
        description={t("toolsMigrate.review.shares.description")}
        rows={rowsOfChecks(rows, SHARE_CHECKS)}
      />
      <TemplatesPreview rows={rows} query={templates} />
    </div>
  );
}
