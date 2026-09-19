import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useParams } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { DetailTabs } from "@/components/patterns/detail-tabs";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { DISK_TAB_CONTENTS, DISK_TAB_OVERVIEW, DISK_TAB_SMART } from "@/hooks/disk-detail-tabs";
import { PATHS } from "@/hooks/paths";
import { hoservaClient, type components } from "@/lib/api/client";
import { formatBytes } from "@/routes/storage-setup/config-preview";

type Disk = components["schemas"]["DiskInventoryEntry"];
type PoolDisk = components["schemas"]["PoolDiskEntry"];

export function DiskDetailPage(): React.ReactElement {
  const { t } = useTranslation();
  const { diskId = "" } = useParams();
  const device = decodeURIComponent(diskId);
  const [disk, setDisk] = useState<Disk | null>(null);
  const [poolDisk, setPoolDisk] = useState<PoolDisk | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    Promise.all([
      hoservaClient.GET("/disks", { signal: controller.signal }),
      hoservaClient.GET("/pool", { signal: controller.signal }),
    ])
      .then(([diskResult, poolResult]) => {
        if (diskResult.error) {
          setError(diskResult.error.message);
          return;
        }
        const found = diskResult.data?.disks.find((entry) => entry.device === device) ?? null;
        setDisk(found);
        setPoolDisk(poolResult.data?.disks.find((entry) => entry.device === device) ?? null);
      })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) {
          setError(err instanceof Error ? err.message : String(err));
        }
      });
    return () => controller.abort();
  }, [device]);

  const smartRows = useMemo(
    () => [
      { id: "status", name: t("diskDetail.smart.status"), value: disk?.smartStatus ?? "—" },
      { id: "model", name: t("diskDetail.smart.model"), value: disk?.model ?? "—" },
      { id: "serial", name: t("diskDetail.smart.serial"), value: disk?.serial ?? "—" },
    ],
    [disk, t],
  );

  if (!disk && !error) {
    return <LoadingBlock />;
  }

  if (!disk) {
    return <Banner tone="error" title={t("diskDetail.notFound", { device })} />;
  }

  const smartColumns: DataTableColumn<{ id: string; name: string; value: string }>[] = [
    { id: "name", header: t("diskDetail.smart.attribute"), cell: (row) => row.name },
    { id: "value", header: t("diskDetail.smart.value"), cell: (row) => row.value },
  ];

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center justify-between gap-2">
        <div>
          <h1 className="text-2xl font-semibold font-heading">{device}</h1>
          <p className="text-muted-foreground">{disk.model ?? t("diskDetail.unknownModel")}</p>
        </div>
        <Button variant="outline" render={<Link to={PATHS.storageDisks} />}>
          {t("diskDetail.back")}
        </Button>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      <DetailTabs
        tabs={[
          {
            id: DISK_TAB_OVERVIEW,
            label: t("diskDetail.tabs.overview"),
            content: (
              <Card>
                <CardHeader>
                  <CardTitle>{t("diskDetail.overview.title")}</CardTitle>
                </CardHeader>
                <CardPanel className="grid gap-2 text-sm sm:grid-cols-2">
                  <p>{t("diskDetail.overview.size", { size: formatBytes(disk.sizeBytes) })}</p>
                  <p>{t("diskDetail.overview.role", { role: poolDisk?.role ?? t("storageSetup.roles.unassigned") })}</p>
                  <p>{t("diskDetail.overview.mount", { mount: poolDisk?.mountPoint ?? "—" })}</p>
                  <p>
                    {t("diskDetail.overview.state")}{" "}
                    <StatusBadge tone={poolDisk?.state === "failed" ? "error" : "success"}>
                      {poolDisk?.state ?? t("status.unknown")}
                    </StatusBadge>
                  </p>
                </CardPanel>
              </Card>
            ),
          },
          {
            id: DISK_TAB_SMART,
            label: t("diskDetail.tabs.smart"),
            content: <DataTable columns={smartColumns} rows={smartRows} getRowKey={(row) => row.id} />,
          },
          {
            id: DISK_TAB_CONTENTS,
            label: t("diskDetail.tabs.contents"),
            content: (
              <Banner
                tone="info"
                title={t("diskDetail.contents.title")}
                description={t("diskDetail.contents.description")}
              />
            ),
          },
        ]}
      />
    </div>
  );
}
