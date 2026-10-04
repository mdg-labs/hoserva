import { useMemo } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import {
  MAPPING_ROLES,
  diskKey,
  diskViolations,
  parityCount,
  parityProblem,
  type DiskMapping,
  type MappingRole,
} from "@/routes/tools-migrate/mapping";
import type { ReviewDisk } from "@/routes/tools-migrate/report";
import { formatBytes } from "@/routes/storage-setup/config-preview";

const UNSET = "unset";

interface MappingRow {
  disk: ReviewDisk;
  key: string;
}

function diskLabel(disk: ReviewDisk): string {
  return disk.serial ?? disk.unraidId ?? disk.slot ?? disk.device ?? "";
}

function RoleSelect({
  row,
  role,
  onChange,
}: {
  row: MappingRow;
  role: MappingRole | null;
  onChange: (key: string, role: MappingRole | null) => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const violations = diskViolations(row.disk, role);
  const items = [
    { value: UNSET, label: t("toolsMigrate.review.mapping.roles.unset") },
    ...MAPPING_ROLES.map((value) => ({ value, label: t(`toolsMigrate.review.mapping.roles.${value}`) })),
  ];

  return (
    <div className="flex flex-col gap-1">
      <Select
        items={items}
        value={role ?? UNSET}
        onValueChange={(value) => onChange(row.key, value === UNSET ? null : (value as MappingRole))}
      >
        <SelectTrigger
          aria-label={t("toolsMigrate.review.mapping.roleFor", { disk: diskLabel(row.disk) })}
          aria-invalid={violations.length > 0}
        >
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {items.map((item) => (
            <SelectItem key={item.value} value={item.value}>
              {item.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {violations.map((violation) => (
        <p key={violation} role="alert" className="text-destructive-foreground text-xs">
          {t(`toolsMigrate.review.mapping.violations.${violation}`)}
        </p>
      ))}
    </div>
  );
}

function DiskCell({ disk }: { disk: ReviewDisk }): React.ReactElement {
  const { t } = useTranslation();
  const detail = [disk.slot, disk.device, disk.model].filter(Boolean).join(" · ");

  return (
    <div className="flex flex-col gap-1">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">{disk.serial ?? disk.unraidId ?? t("toolsMigrate.review.mapping.unknown")}</span>
        {disk.refused ? <StatusBadge tone="error">{t("toolsMigrate.review.mapping.refused")}</StatusBadge> : null}
        {disk.weakIdentity === true ? (
          <StatusBadge tone="warning">{t("toolsMigrate.review.mapping.weakIdentity")}</StatusBadge>
        ) : null}
        {disk.unraidBoot === true || disk.unraidRole === "boot" ? (
          <StatusBadge tone="info">{t("toolsMigrate.review.mapping.unraidBoot")}</StatusBadge>
        ) : null}
        {disk.slot === undefined ? (
          <StatusBadge tone="outline">{t("toolsMigrate.review.mapping.notInCapture")}</StatusBadge>
        ) : null}
      </div>
      {detail ? <span className="text-muted-foreground text-xs">{detail}</span> : null}
      {disk.refusal ? <span className="whitespace-normal text-muted-foreground text-xs">{disk.refusal}</span> : null}
      {disk.problem ? (
        <span className="whitespace-normal text-muted-foreground text-xs">
          {t("toolsMigrate.review.mapping.notFound", { problem: disk.problem })}
        </span>
      ) : null}
    </div>
  );
}

export function MappingTable({
  disks,
  mapping,
  onChange,
}: {
  disks: ReviewDisk[];
  mapping: DiskMapping;
  onChange: (key: string, role: MappingRole | null) => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const rows = useMemo<MappingRow[]>(() => disks.map((disk, index) => ({ disk, key: diskKey(disk, index) })), [disks]);
  const unknown = t("toolsMigrate.review.mapping.unknown");

  const columns: DataTableColumn<MappingRow>[] = [
    { id: "serial", header: t("toolsMigrate.review.mapping.columns.serial"), cell: (row) => <DiskCell disk={row.disk} />, className: "whitespace-normal" },
    {
      id: "diskNumber",
      header: t("toolsMigrate.review.mapping.columns.diskNumber"),
      cell: (row) => row.disk.diskNumber ?? t("toolsMigrate.review.mapping.noNumber"),
    },
    {
      id: "size",
      header: t("toolsMigrate.review.mapping.columns.size"),
      cell: (row) => (row.disk.size === undefined ? unknown : formatBytes(row.disk.size)),
    },
    {
      id: "filesystem",
      header: t("toolsMigrate.review.mapping.columns.filesystem"),
      cell: (row) => row.disk.filesystem ?? unknown,
    },
    {
      id: "role",
      header: t("toolsMigrate.review.mapping.columns.role"),
      cell: (row) => <RoleSelect row={row} role={mapping[row.key] ?? null} onChange={onChange} />,
      className: "whitespace-normal",
    },
  ];

  const problem = parityProblem(parityCount(disks, mapping));

  return (
    <div className="flex flex-col gap-3">
      {problem ? (
        <Banner
          tone="warning"
          title={t(`toolsMigrate.review.mapping.parity.${problem}.title`)}
          description={t(`toolsMigrate.review.mapping.parity.${problem}.description`)}
        />
      ) : null}
      <DataTable rows={rows} getRowKey={(row) => row.key} columns={columns} />
    </div>
  );
}
