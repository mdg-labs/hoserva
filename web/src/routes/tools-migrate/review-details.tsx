import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";

import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { PlainTerm } from "@/components/patterns/plain-term";
import { StatusBadge } from "@/components/patterns/status-badge";
import { diskKey, type DiskMapping } from "@/routes/tools-migrate/mapping";
import { ROLLBACK_ROWS, bootKind, type MigrationBoot, type ReviewDisk, type SharePreview } from "@/routes/tools-migrate/report";

const HIGH_WATER_POLICY = "mfs";

function allocationLabel(share: SharePreview, t: TFunction): React.ReactNode {
  if (share.highWater) {
    return <PlainTerm label={t("toolsMigrate.review.shares.allocation.highwater")} term={HIGH_WATER_POLICY} />;
  }
  if (share.allocationMethod === undefined) {
    return t("toolsMigrate.review.shares.allocation.unset");
  }
  return t(`toolsMigrate.review.shares.allocation.${share.allocationMethod}`, { defaultValue: share.allocationMethod });
}

function disksLabel(share: SharePreview, t: TFunction): string {
  const parts: string[] = [];
  if (share.include.length > 0) {
    parts.push(t("toolsMigrate.review.shares.only", { disks: share.include.join(", ") }));
  }
  if (share.exclude.length > 0) {
    parts.push(t("toolsMigrate.review.shares.except", { disks: share.exclude.join(", ") }));
  }
  return parts.length > 0 ? parts.join("; ") : t("toolsMigrate.review.shares.anyDisk");
}

export function SharesPreview({ shares }: { shares: SharePreview[] }): React.ReactElement {
  const { t } = useTranslation();
  const columns: DataTableColumn<SharePreview>[] = [
    { id: "name", header: t("toolsMigrate.review.shares.columns.name"), cell: (share) => <span className="font-medium">{share.name}</span> },
    {
      id: "allocation",
      header: t("toolsMigrate.review.shares.columns.allocation"),
      cell: (share) => allocationLabel(share, t),
      className: "whitespace-normal",
    },
    { id: "disks", header: t("toolsMigrate.review.shares.columns.disks"), cell: (share) => disksLabel(share, t), className: "whitespace-normal" },
    {
      id: "warnings",
      header: t("toolsMigrate.review.shares.columns.warnings"),
      cell: (share) =>
        share.warningCount > 0 ? (
          <StatusBadge tone="warning">{t("toolsMigrate.review.shares.warningCount", { count: share.warningCount })}</StatusBadge>
        ) : (
          <StatusBadge tone="success">{t("toolsMigrate.review.shares.noWarnings")}</StatusBadge>
        ),
    },
  ];

  return (
    <section className="flex flex-col gap-2" aria-label={t("toolsMigrate.review.shares.title")}>
      <h3 className="font-medium">{t("toolsMigrate.review.shares.title")}</h3>
      <p className="text-muted-foreground text-sm">{t("toolsMigrate.review.shares.previewDescription")}</p>
      {shares.length > 0 ? (
        <DataTable rows={shares} getRowKey={(share) => share.name} columns={columns} />
      ) : (
        <p className="text-muted-foreground text-sm">{t("toolsMigrate.review.shares.none")}</p>
      )}
      {shares.some((share) => share.highWater) ? (
        <p className="text-muted-foreground text-sm">{t("toolsMigrate.review.shares.highWaterNote")}</p>
      ) : null}
    </section>
  );
}

export function BootLayout({
  boot,
  disks,
  mapping,
}: {
  boot: MigrationBoot;
  disks: ReviewDisk[];
  mapping: DiskMapping;
}): React.ReactElement {
  const { t } = useTranslation();
  const kind = bootKind(boot);
  const cacheDisks = disks
    .map((disk, index) => ({ disk, role: mapping[diskKey(disk, index)] }))
    .filter((entry) => entry.role === "cache")
    .map((entry) => entry.disk.serial ?? entry.disk.device ?? entry.disk.unraidId ?? entry.disk.slot ?? "")
    .filter((label) => label !== "");
  const rows = ROLLBACK_ROWS[kind];
  const columns: DataTableColumn<string>[] = [
    {
      id: "debian",
      header: t("toolsMigrate.review.boot.columns.debian"),
      cell: (row) => t(`toolsMigrate.review.boot.rollbackRows.${kind}.${row}.debian`),
      className: "whitespace-normal",
    },
    {
      id: "rollback",
      header: t("toolsMigrate.review.boot.columns.rollback"),
      cell: (row) => t(`toolsMigrate.review.boot.rollbackRows.${kind}.${row}.rollback`),
      className: "whitespace-normal",
    },
  ];

  return (
    <section className="flex flex-col gap-2" aria-label={t("toolsMigrate.review.boot.layoutTitle")}>
      <h3 className="font-medium">{t("toolsMigrate.review.boot.layoutTitle")}</h3>
      <p className="text-muted-foreground text-sm">{t("toolsMigrate.review.boot.layoutDescription")}</p>
      <dl className="grid gap-1 text-sm sm:grid-cols-[auto_1fr] sm:gap-x-4">
        <dt className="text-muted-foreground">{t("toolsMigrate.review.boot.unraidBoots")}</dt>
        <dd>{t(`toolsMigrate.review.boot.kinds.${kind}`)}</dd>
        <dt className="text-muted-foreground">{t("toolsMigrate.review.boot.plannedCache")}</dt>
        <dd>
          {cacheDisks.length > 0
            ? t("toolsMigrate.review.boot.cacheOn", { disks: cacheDisks.join(", ") })
            : t("toolsMigrate.review.boot.noCache")}
        </dd>
      </dl>
      {rows.length > 0 ? (
        <>
          <p className="text-sm">{t("toolsMigrate.review.boot.rollbackIntro")}</p>
          <DataTable rows={rows} getRowKey={(row) => row} columns={columns} />
          <p className="text-muted-foreground text-sm">{t("toolsMigrate.review.boot.sharedNvmeNote")}</p>
        </>
      ) : (
        <p className="text-sm">{t(`toolsMigrate.review.boot.noRollback.${kind}`)}</p>
      )}
    </section>
  );
}
