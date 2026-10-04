import { HardDrive } from "lucide-react";
import { useTranslation } from "react-i18next";

import { DataTable } from "@/components/patterns/data-table";
import { EmptyState } from "@/components/patterns/empty-state";
import { Banner } from "@/components/patterns/banner";
import { Button } from "@/components/ui/button";
import { listMigrationTemplates } from "@/lib/api/operations";
import { useApiQuery } from "@/lib/api/use-api-query";
import { CaptureWarningBanner } from "@/routes/tools-migrate/banners";
import { MappingTable } from "@/routes/tools-migrate/mapping-table";
import type { DiskMapping, MappingRole } from "@/routes/tools-migrate/mapping";
import { BootLayout, SharesPreview } from "@/routes/tools-migrate/review-details";
import {
  BOOT_CHECKS,
  DISK_CHECKS,
  SHARE_CHECKS,
  allTemplatesUnknown,
  captureNotice,
  hasMatchedDisks,
  reportRowKey,
  rowsOfChecks,
  type MigrationReport,
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
  report,
  mapping,
  onMappingChange,
  onScanAgain,
}: {
  report: MigrationReport;
  mapping: DiskMapping | undefined;
  onMappingChange: (key: string, role: MappingRole | null) => void;
  onScanAgain: () => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const rows = report.rows;
  const review = report.review;
  const matched = hasMatchedDisks(rows);
  const templates = useApiQuery({
    queryKey: "migration-templates",
    queryFn: (signal) => listMigrationTemplates(signal),
    fallbackError: t("toolsMigrate.templates.loadFailed"),
  });
  const notice = captureNotice(report);

  return (
    <div className="flex flex-col gap-6">
      {notice ? (
        <CaptureWarningBanner
          notice={notice}
          allUnknown={templates.data !== null && allTemplatesUnknown(templates.data)}
        />
      ) : null}

      {review ? null : (
        <Banner
          tone="info"
          title={t("toolsMigrate.review.noReview.title")}
          description={t("toolsMigrate.review.noReview.description")}
          action={
            <Button size="xs" variant="outline" onClick={onScanAgain}>
              {t("toolsMigrate.actions.scanAgain")}
            </Button>
          }
        />
      )}

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

      {review && mapping ? (
        <section className="flex flex-col gap-2" aria-label={t("toolsMigrate.review.mapping.title")}>
          <h3 className="font-medium">{t("toolsMigrate.review.mapping.title")}</h3>
          <p className="text-muted-foreground text-sm">{t("toolsMigrate.review.mapping.description")}</p>
          {review.disks.length > 0 ? (
            <MappingTable disks={review.disks} mapping={mapping} onChange={onMappingChange} />
          ) : (
            <p className="text-muted-foreground text-sm">{t("toolsMigrate.review.noRows")}</p>
          )}
        </section>
      ) : (
        <RowSection
          title={t("toolsMigrate.review.disks.title")}
          description={t("toolsMigrate.review.disks.description")}
          rows={rowsOfChecks(rows, DISK_CHECKS)}
        />
      )}

      {review && mapping ? <BootLayout boot={review.boot} disks={review.disks} mapping={mapping} /> : null}
      <RowSection
        title={t("toolsMigrate.review.boot.title")}
        description={t("toolsMigrate.review.boot.description")}
        rows={rowsOfChecks(rows, BOOT_CHECKS)}
      />

      {review ? (
        <SharesPreview shares={review.shares} />
      ) : (
        <RowSection
          title={t("toolsMigrate.review.shares.title")}
          description={t("toolsMigrate.review.shares.description")}
          rows={rowsOfChecks(rows, SHARE_CHECKS)}
        />
      )}
      <TemplatesPreview rows={rows} query={templates} />
    </div>
  );
}
