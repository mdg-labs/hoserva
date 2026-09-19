import type { TFunction } from "i18next";
import { SkipForward } from "lucide-react";
import type { LucideIcon } from "lucide-react";

import type { DataTableColumn } from "@/components/patterns/data-table";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Field, FieldError } from "@/components/ui/field";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { formatBytes } from "@/routes/storage-setup/config-preview";
import type { DiskEntry, DiskRole } from "@/routes/storage-setup/validation";

const COLUMN_DEVICE = "device";
const COLUMN_MODEL = "model";
const COLUMN_SERIAL = "serial";
const COLUMN_SIZE = "size";
const COLUMN_FILESYSTEM = "filesystem";
const COLUMN_LABEL = "label";
const COLUMN_SMART = "smart";
const COLUMN_CONTAINS_DATA = "containsData";
const COLUMN_UNRAID = "unraid";
const COLUMN_STATUS = "status";
const COLUMN_ROLE = "role";
const ROLE_UNASSIGNED = "unassigned";

function diskHasExtra(disk: DiskEntry, key: keyof DiskEntry): boolean {
  return disk[key] !== undefined && disk[key] !== null && disk[key] !== "";
}

export function discoveryOptionalColumns(disks: DiskEntry[]): {
  filesystem: boolean;
  label: boolean;
  smart: boolean;
  containsData: boolean;
  unraid: boolean;
} {
  return {
    filesystem: disks.some((disk) => diskHasExtra(disk, "filesystem")),
    label: disks.some((disk) => diskHasExtra(disk, "label")),
    smart: disks.some((disk) => diskHasExtra(disk, "smartStatus") || disk.failed !== undefined),
    containsData: disks.some((disk) => diskHasExtra(disk, "containsData")),
    unraid: disks.some((disk) => diskHasExtra(disk, "looksLikeUnraid")),
  };
}

export function buildDiscoveryColumns(
  t: TFunction,
  optional: ReturnType<typeof discoveryOptionalColumns>,
): DataTableColumn<DiskEntry>[] {
  const columns: DataTableColumn<DiskEntry>[] = [
    { id: COLUMN_DEVICE, header: t("storageSetup.columns.device"), cell: (disk) => disk.device },
    { id: COLUMN_MODEL, header: t("storageSetup.columns.model"), cell: (disk) => disk.model ?? "—" },
    { id: COLUMN_SERIAL, header: t("storageSetup.columns.serial"), cell: (disk) => disk.serial ?? "—" },
    { id: COLUMN_SIZE, header: t("storageSetup.columns.size"), cell: (disk) => formatBytes(disk.sizeBytes) },
  ];

  if (optional.filesystem) {
    columns.push({
      id: COLUMN_FILESYSTEM,
      header: t("storageSetup.columns.filesystem"),
      cell: (disk) => disk.filesystem ?? "—",
    });
  }
  if (optional.label) {
    columns.push({
      id: COLUMN_LABEL,
      header: t("storageSetup.columns.label"),
      cell: (disk) => disk.label ?? "—",
    });
  }
  if (optional.smart) {
    columns.push({
      id: COLUMN_SMART,
      header: t("storageSetup.columns.smart"),
      cell: (disk) => (
        <StatusBadge tone={disk.failed ? "error" : "success"}>
          {disk.smartStatus ?? (disk.failed ? t("storageSetup.smart.failed") : t("storageSetup.smart.ok"))}
        </StatusBadge>
      ),
    });
  }
  if (optional.containsData) {
    columns.push({
      id: COLUMN_CONTAINS_DATA,
      header: t("storageSetup.columns.containsData"),
      cell: (disk) => (disk.containsData ? t("common.yes") : t("common.no")),
    });
  }
  if (optional.unraid) {
    columns.push({
      id: COLUMN_UNRAID,
      header: t("storageSetup.columns.unraid"),
      cell: (disk) => (disk.looksLikeUnraid ? t("common.yes") : t("common.no")),
    });
  }

  columns.push({
    id: COLUMN_STATUS,
    header: t("storageSetup.columns.status"),
    cell: (disk) =>
      disk.boot ? (
        <StatusBadge tone="outline">{t("storageSetup.discovery.bootDisk")}</StatusBadge>
      ) : disk.weakIdentity ? (
        <StatusBadge tone="warning">{t("storageSetup.discovery.weakIdentity")}</StatusBadge>
      ) : (
        <StatusBadge tone="success">{t("storageSetup.discovery.available")}</StatusBadge>
      ),
  });

  return columns;
}

export function buildRoleColumns(
  t: TFunction,
  roles: Record<string, DiskRole>,
  roleIcons: Record<Exclude<DiskRole, "unassigned">, LucideIcon>,
  onRoleChange: (device: string, role: DiskRole) => void,
  roleErrorMessage: (device: string) => string | undefined,
  roleWarningMessage: (device: string) => string | undefined,
): DataTableColumn<DiskEntry>[] {
  return [
    { id: COLUMN_DEVICE, header: t("storageSetup.columns.device"), cell: (disk) => disk.device },
    { id: COLUMN_SIZE, header: t("storageSetup.columns.size"), cell: (disk) => formatBytes(disk.sizeBytes) },
    {
      id: COLUMN_ROLE,
      header: t("storageSetup.columns.role"),
      cell: (disk) => {
        const role = roles[disk.device] ?? ROLE_UNASSIGNED;
        const fieldError = roleErrorMessage(disk.device);
        const fieldWarning = roleWarningMessage(disk.device);
        return (
          <Field>
            <Select value={role} onValueChange={(value) => value && onRoleChange(disk.device, value as DiskRole)}>
              <SelectTrigger aria-label={t("storageSetup.columns.role")}>
                <SelectValue />
              </SelectTrigger>
              <SelectPopup>
                {(Object.keys(roleIcons) as Array<Exclude<DiskRole, "unassigned">>).map((option) => {
                  const Icon = roleIcons[option];
                  const disabled = option === "parity" && disk.weakIdentity;
                  return (
                    <SelectItem key={option} value={option} disabled={disabled}>
                      <span className="flex items-center gap-2">
                        <Icon aria-hidden className="size-4" />
                        {t(`storageSetup.roles.${option}`)}
                      </span>
                    </SelectItem>
                  );
                })}
                <SelectItem value={ROLE_UNASSIGNED}>
                  <span className="flex items-center gap-2">
                    <SkipForward aria-hidden className="size-4" />
                    {t("storageSetup.roles.unassigned")}
                  </span>
                </SelectItem>
              </SelectPopup>
            </Select>
            {fieldError ? <FieldError>{fieldError}</FieldError> : null}
            {!fieldError && fieldWarning ? (
              <p className="text-warning-foreground text-xs">{fieldWarning}</p>
            ) : null}
          </Field>
        );
      },
    },
  ];
}
