import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import { MetricTile } from "@/components/patterns/metric-tile";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Button } from "@/components/ui/button";
import { useSystemData } from "@/hooks/use-system-status";
import { formatBytes } from "@/routes/storage-setup/config-preview";

export function PoolOverviewPage(): React.ReactElement {
  const { t } = useTranslation();
  const { status, pool, loading, error } = useSystemData();
  const [stopOpen, setStopOpen] = useState(false);
  const [startOpen, setStartOpen] = useState(false);
  const maintenance = Boolean(status?.maintenanceMode);

  if (loading) {
    return <LoadingBlock />;
  }

  const disks = pool?.disks ?? [];
  const dataDisks = disks.filter((disk) => disk.role === "data");
  const total = disks.reduce((sum, disk) => sum + (disk.sizeBytes ?? 0), 0);
  const used = disks.reduce((sum, disk) => sum + (disk.usedBytes ?? 0), 0);
  const usedPercent = total > 0 ? Math.round((used / total) * 100) : 0;

  const stopItems = [
    t("pool.stop.items.jobs"),
    t("pool.stop.items.vms"),
    t("pool.stop.items.containers"),
    t("pool.stop.items.shares"),
    t("pool.stop.items.mounts"),
  ];

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 className="text-2xl font-semibold font-heading">{t("storageNav.pool")}</h1>
          <p className="text-muted-foreground">{t("pool.description")}</p>
        </div>
        <div className="flex gap-2">
          {maintenance ? (
            <Button variant="default" onClick={() => setStartOpen(true)}>
              {t("pool.startArray")}
            </Button>
          ) : (
            <Button variant="destructive-outline" onClick={() => setStopOpen(true)}>
              {t("pool.stopArray")}
            </Button>
          )}
        </div>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      <InlineNote description={t("pool.noRebuildNote")} />
      <div className="grid gap-4 md:grid-cols-2">
        <MetricTile
          title={t("pool.capacity")}
          value={`${formatBytes(used)} / ${formatBytes(total)}`}
          description={t("pool.mountPoint", { path: "/mnt/user" })}
          progress={usedPercent}
          footer={
            <StatusBadge tone={pool?.mounted ? "success" : "error"}>
              {pool?.mounted ? t("pool.mounted") : t("pool.unmounted")}
            </StatusBadge>
          }
        />
        <MetricTile
          title={t("pool.diskCount")}
          value={String(dataDisks.length)}
          description={t("pool.dataDisks")}
        />
      </div>
      <section className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {disks.map((disk) => {
          const size = disk.sizeBytes ?? 0;
          const diskUsed = disk.usedBytes ?? 0;
          const percent = size > 0 ? Math.round((diskUsed / size) * 100) : 0;
          return (
            <MetricTile
              key={disk.device}
              title={disk.mountPoint}
              value={disk.device}
              description={`${disk.role} · ${formatBytes(diskUsed)}`}
              progress={percent}
              footer={<StatusBadge tone={disk.state === "failed" ? "error" : "success"}>{disk.state}</StatusBadge>}
            />
          );
        })}
      </section>
      <ConfirmDialog
        open={stopOpen}
        onOpenChange={setStopOpen}
        title={t("pool.stop.title")}
        description={t("pool.stop.description")}
        items={stopItems}
        confirmLabel={t("pool.stop.confirm")}
        destructive
        onConfirm={() => setStopOpen(false)}
      />
      <ConfirmDialog
        open={startOpen}
        onOpenChange={setStartOpen}
        title={t("pool.start.title")}
        description={t("pool.start.description")}
        confirmLabel={t("pool.start.confirm")}
        onConfirm={() => setStartOpen(false)}
      />
    </div>
  );
}
