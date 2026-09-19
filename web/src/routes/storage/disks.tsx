import { HardDrive } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { EmptyState } from "@/components/patterns/empty-state";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { SelectFilter, TableFilters } from "@/components/patterns/table-filters";
import { Button } from "@/components/ui/button";
import { DISK_FILTER_ALL, DISK_ROLE_FILTER_VALUES } from "@/hooks/disk-filter-options";
import { diskDetailPath, PATHS } from "@/hooks/paths";
import { hoservaClient, type components } from "@/lib/api/client";
import { formatBytes } from "@/routes/storage-setup/config-preview";

type Disk = components["schemas"]["DiskInventoryEntry"];
type PoolDisk = components["schemas"]["PoolDiskEntry"];

export function DisksPage(): React.ReactElement {
  const { t } = useTranslation();
  const [disks, setDisks] = useState<Disk[] | null>(null);
  const [poolDisks, setPoolDisks] = useState<PoolDisk[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [roleFilter, setRoleFilter] = useState(DISK_FILTER_ALL);

  useEffect(() => {
    const controller = new AbortController();
    Promise.all([hoservaClient.GET("/disks", { signal: controller.signal }), hoservaClient.GET("/pool", { signal: controller.signal })])
      .then(([diskResult, poolResult]) => {
        if (diskResult.error) {
          setError(diskResult.error.message);
          return;
        }
        setDisks(diskResult.data?.disks ?? []);
        setPoolDisks(poolResult.data?.disks ?? []);
      })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) {
          setError(err instanceof Error ? err.message : String(err));
        }
      });
    return () => controller.abort();
  }, []);

  const poolByDevice = useMemo(
    () => new Map(poolDisks.map((disk) => [disk.device, disk])),
    [poolDisks],
  );

  const rows = useMemo(() => {
    if (!disks) return [];
    return disks.filter((disk) => {
      const haystack = `${disk.device} ${disk.model ?? ""} ${disk.serial ?? ""}`.toLowerCase();
      if (search && !haystack.includes(search.toLowerCase())) return false;
      const role = poolByDevice.get(disk.device)?.role ?? "unassigned";
      if (roleFilter !== DISK_FILTER_ALL && role !== roleFilter) return false;
      return true;
    });
  }, [disks, poolByDevice, roleFilter, search]);

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
      cell: (disk) => poolByDevice.get(disk.device)?.role ?? t("storageSetup.roles.unassigned"),
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
      cell: (disk) => poolByDevice.get(disk.device)?.state ?? "—",
    },
  ];

  if (disks === null && !error) {
    return <LoadingBlock />;
  }

  return (
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
  );
}
