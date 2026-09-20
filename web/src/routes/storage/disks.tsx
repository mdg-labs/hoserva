import { HardDrive, MoreHorizontal } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { EmptyState } from "@/components/patterns/empty-state";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { SelectFilter, TableFilters } from "@/components/patterns/table-filters";
import { TypedConfirm } from "@/components/patterns/typed-confirm";
import { Button } from "@/components/ui/button";
import { Menu, MenuContent, MenuItem, MenuTrigger } from "@/components/ui/menu";
import { Switch } from "@/components/ui/switch";
import { DISK_FILTER_ALL, DISK_ROLE_FILTER_VALUES } from "@/hooks/disk-filter-options";
import { diskDetailPath, PATHS } from "@/hooks/paths";
import { hoservaClient, type components } from "@/lib/api/client";
import { formatBytes } from "@/routes/storage-setup/config-preview";

type Disk = components["schemas"]["DiskInventoryEntry"];
type PoolDisk = components["schemas"]["PoolDiskEntry"];
type ExternalDisk = components["schemas"]["ExternalDisk"];

function erasePhrase(device: string): string {
  return `ERASE ${device}`;
}

export function DisksPage(): React.ReactElement {
  const { t } = useTranslation();
  const [disks, setDisks] = useState<Disk[] | null>(null);
  const [poolDisks, setPoolDisks] = useState<PoolDisk[] | null>(null);
  const [externalDisks, setExternalDisks] = useState<ExternalDisk[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [roleFilter, setRoleFilter] = useState(DISK_FILTER_ALL);
  const [busyLabel, setBusyLabel] = useState<string | null>(null);
  const [formatTarget, setFormatTarget] = useState<ExternalDisk | null>(null);
  const [formatConfirm, setFormatConfirm] = useState("");

  const load = useCallback((signal?: AbortSignal) => {
    return Promise.all([
      hoservaClient.GET("/disks", { signal }),
      hoservaClient.GET("/pool", { signal }),
      hoservaClient.GET("/disks/external", { signal }),
    ]).then(([diskResult, poolResult, externalResult]) => {
      if (diskResult.error) {
        setError(diskResult.error.message);
        return;
      }
      setDisks(diskResult.data?.disks ?? []);
      if (poolResult.error) {
        setError(poolResult.error.message);
        return;
      }
      setPoolDisks(poolResult.data?.disks ?? []);
      if (externalResult.error) {
        setError(externalResult.error.message);
        return;
      }
      setExternalDisks(externalResult.data?.disks ?? []);
      setError(null);
    });
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    load(controller.signal).catch((err: unknown) => {
      if (!controller.signal.aborted) {
        setError(err instanceof Error ? err.message : String(err));
      }
    });
    return () => controller.abort();
  }, [load]);

  const poolByDevice = useMemo(
    () => new Map((poolDisks ?? []).map((disk) => [disk.device, disk])),
    [poolDisks],
  );
  const externalDevices = useMemo(
    () => new Set((externalDisks ?? []).map((disk) => disk.device)),
    [externalDisks],
  );

  const rows = useMemo(() => {
    if (!disks) return [];
    return disks.filter((disk) => {
      if (externalDevices.has(disk.device)) return false;
      const haystack = `${disk.device} ${disk.model ?? ""} ${disk.serial ?? ""}`.toLowerCase();
      if (search && !haystack.includes(search.toLowerCase())) return false;
      const role = poolByDevice.get(disk.device)?.role ?? (poolDisks === null ? "unknown" : "unassigned");
      if (roleFilter !== DISK_FILTER_ALL && role !== roleFilter) return false;
      return true;
    });
  }, [disks, externalDevices, poolByDevice, poolDisks, roleFilter, search]);

  async function runExternal(
    label: string,
    action: () => Promise<{ error?: { message: string } }>,
  ): Promise<void> {
    setBusyLabel(label);
    setError(null);
    try {
      const result = await action();
      if (result.error) {
        setError(result.error.message);
        return;
      }
      await load();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusyLabel(null);
    }
  }

  function setBackupDestination(label: string, backupDestination: boolean): void {
    void runExternal(label, () =>
      hoservaClient.PATCH("/disks/external/{label}", {
        params: { path: { label } },
        body: { backupDestination },
      }),
    );
  }

  function mountExternal(label: string): void {
    void runExternal(label, () =>
      hoservaClient.POST("/disks/external/{label}/mount", {
        params: { path: { label } },
      }),
    );
  }

  function ejectExternal(label: string): void {
    void runExternal(label, () =>
      hoservaClient.POST("/disks/external/{label}/eject", {
        params: { path: { label } },
      }),
    );
  }

  function formatExternal(label: string, confirmation: string): Promise<void> {
    return runExternal(label, () =>
      hoservaClient.POST("/disks/external/{label}/format", {
        params: { path: { label } },
        body: { confirmation },
      }),
    );
  }

  const columns: DataTableColumn<Disk>[] = [
    {
      id: "device",
      header: t("disks.columns.device"),
      cell: (disk) => (
        <Link to={diskDetailPath(disk.device)} className="font-medium">
          {disk.device}
        </Link>
      ),
    },
    { id: "model", header: t("disks.columns.model"), cell: (disk) => disk.model ?? "—" },
    { id: "serial", header: t("disks.columns.serial"), cell: (disk) => disk.serial ?? "—" },
    {
      id: "role",
      header: t("disks.columns.role"),
      cell: (disk) =>
        poolByDevice.get(disk.device)?.role ??
        (poolDisks === null ? t("status.unknown") : t("storageSetup.roles.unassigned")),
    },
    { id: "size", header: t("disks.columns.size"), cell: (disk) => formatBytes(disk.sizeBytes) },
    {
      id: "smart",
      header: t("disks.columns.smart"),
      cell: (disk) => (
        <StatusBadge tone={disk.smartStatus === "failing" ? "error" : "success"}>
          {disk.smartStatus ?? t("status.unknown")}
        </StatusBadge>
      ),
    },
    {
      id: "state",
      header: t("disks.columns.state"),
      cell: (disk) =>
        poolByDevice.get(disk.device)?.state ?? (poolDisks === null ? t("status.unknown") : "—"),
    },
  ];

  const externalColumns: DataTableColumn<ExternalDisk>[] = [
    {
      id: "label",
      header: t("disks.external.columns.label"),
      cell: (disk) => <span className="font-medium">{disk.label}</span>,
    },
    { id: "device", header: t("disks.columns.device"), cell: (disk) => disk.device },
    { id: "mount", header: t("disks.external.columns.mount"), cell: (disk) => disk.mountPoint },
    {
      id: "container",
      header: t("disks.external.columns.containerPath"),
      cell: (disk) => disk.containerPath,
    },
    {
      id: "mounted",
      header: t("disks.external.columns.mounted"),
      cell: (disk) => (
        <StatusBadge tone={disk.mounted ? "success" : "outline"}>
          {disk.mounted ? t("disks.external.mounted") : t("disks.external.unmounted")}
        </StatusBadge>
      ),
    },
    {
      id: "backup",
      header: t("disks.external.columns.backup"),
      cell: (disk) => (
        <Switch
          checked={disk.backupDestination}
          disabled={busyLabel === disk.label}
          aria-label={t("disks.external.columns.backup")}
          onCheckedChange={(checked) => {
            setBackupDestination(disk.label, checked);
          }}
        />
      ),
    },
    {
      id: "actions",
      header: t("disks.external.columns.actions"),
      cell: (disk) => (
        <Menu>
          <MenuTrigger
            render={
              <Button
                size="icon-sm"
                variant="ghost"
                aria-label={t("disks.external.actions.menu", { label: disk.label })}
                disabled={busyLabel === disk.label}
              >
                <MoreHorizontal />
              </Button>
            }
          />
          <MenuContent>
            <MenuItem
              disabled={disk.mounted}
              onClick={() => {
                mountExternal(disk.label);
              }}
            >
              {t("disks.external.actions.mount")}
            </MenuItem>
            <MenuItem
              disabled={!disk.mounted}
              onClick={() => {
                ejectExternal(disk.label);
              }}
            >
              {t("disks.external.actions.eject")}
            </MenuItem>
            <MenuItem
              disabled={disk.mounted}
              onClick={() => {
                setFormatConfirm("");
                setFormatTarget(disk);
              }}
            >
              {t("disks.external.actions.format")}
            </MenuItem>
          </MenuContent>
        </Menu>
      ),
    },
  ];

  if (disks === null && !error) {
    return <LoadingBlock />;
  }

  const phrase = formatTarget ? erasePhrase(formatTarget.device) : "";

  return (
    <div className="flex flex-col gap-8">
      <div className="flex flex-col gap-4">
        <div>
          <h1 className="text-2xl font-semibold font-heading">{t("storageNav.disks")}</h1>
          <p className="text-muted-foreground">{t("disks.description")}</p>
        </div>
        {error ? <Banner tone="error" title={error} /> : null}
        <TableFilters
          search={search}
          onSearchChange={setSearch}
          searchPlaceholder={t("disks.search")}
          filters={
            <SelectFilter
              value={roleFilter}
              onChange={setRoleFilter}
              placeholder={t("disks.roleFilter")}
              options={[
                { value: DISK_FILTER_ALL, label: t("disks.roles.all") },
                ...DISK_ROLE_FILTER_VALUES.map((value) => ({
                  value,
                  label: t(`storageSetup.roles.${value}`),
                })),
              ]}
            />
          }
        />
        {rows.length === 0 ? (
          <EmptyState icon={HardDrive} title={t("disks.empty.title")} description={t("disks.empty.description")} />
        ) : (
          <DataTable columns={columns} rows={rows} getRowKey={(disk) => disk.device} />
        )}
        <Button variant="outline" render={<Link to={PATHS.storageWakeEvents} />}>
          {t("disks.viewWakeEvents")}
        </Button>
      </div>

      <div className="flex flex-col gap-4">
        <div>
          <h2 className="text-xl font-semibold font-heading">{t("disks.external.title")}</h2>
          <p className="text-muted-foreground">{t("disks.external.description")}</p>
        </div>
        {(externalDisks ?? []).length === 0 ? (
          <EmptyState
            icon={HardDrive}
            title={t("disks.external.empty.title")}
            description={t("disks.external.empty.description")}
          />
        ) : (
          <DataTable columns={externalColumns} rows={externalDisks ?? []} getRowKey={(disk) => disk.label} />
        )}
      </div>

      <FormOverlay
        open={formatTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setFormatTarget(null);
            setFormatConfirm("");
          }
        }}
        title={t("disks.external.format.title")}
        description={t("disks.external.format.description")}
        footer={
          <Button
            variant="destructive"
            disabled={formatTarget === null || formatConfirm !== phrase || busyLabel !== null}
            onClick={() => {
              if (!formatTarget) return;
              const target = formatTarget;
              void formatExternal(target.label, phrase).then(() => {
                setFormatTarget(null);
                setFormatConfirm("");
              });
            }}
          >
            {t("disks.external.format.submit")}
          </Button>
        }
      >
        {formatTarget ? (
          <TypedConfirm
            phrase={phrase}
            value={formatConfirm}
            onChange={setFormatConfirm}
            title={t("disks.external.format.confirmTitle")}
            description={t("disks.external.format.confirmDescription")}
            items={[t("disks.external.format.eraseItem", { device: formatTarget.device })]}
          />
        ) : null}
      </FormOverlay>
    </div>
  );
}
