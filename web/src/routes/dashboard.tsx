import type React from "react";
import { HardDrive } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { TimeSeriesChart } from "@/components/patterns/chart";
import { EmptyState } from "@/components/patterns/empty-state";
import { JobProgress } from "@/components/patterns/job-progress";
import { LoadingBlock } from "@/components/patterns/loading";
import { MetricTile } from "@/components/patterns/metric-tile";
import { StatusBadge } from "@/components/patterns/status-badge";
import {
  arrayStatusLabel,
  arrayStatusTone,
  parityFreshnessLabel,
} from "@/components/patterns/system-status";
import { ScrollArea } from "@/components/ui/scroll-area";
import { useActiveJobs } from "@/hooks/use-active-jobs";
import { diskDetailPath, PATHS } from "@/hooks/paths";
import { useSystemData } from "@/hooks/use-system-status";
import { formatBytes } from "@/routes/storage-setup/config-preview";

function poolSummary(pool: ReturnType<typeof useSystemData>["pool"]): {
  total: number;
  used: number;
  dataCount: number;
  parityCount: number;
  cacheCount: number;
} {
  const disks = pool?.disks ?? [];
  let total = 0;
  let used = 0;
  let dataCount = 0;
  let parityCount = 0;
  let cacheCount = 0;

  for (const disk of disks) {
    if (disk.role === "data") dataCount += 1;
    if (disk.role === "parity") parityCount += 1;
    if (disk.role === "cache") cacheCount += 1;
    if (disk.sizeBytes != null) total += disk.sizeBytes;
    if (disk.usedBytes != null) used += disk.usedBytes;
  }

  return { total, used, dataCount, parityCount, cacheCount };
}

export function DashboardPage(): React.ReactElement {
  const { t } = useTranslation();
  const { status, pool, jobs, doctor, loading, error } = useSystemData();
  const activeJobs = useActiveJobs(jobs);
  const summary = poolSummary(pool);
  const parity = parityFreshnessLabel(status, doctor, t);
  const attention: React.ReactElement[] = [];

  if (status?.arrayDegraded) {
    attention.push(
      <Banner key="degraded" tone="error" title={t("dashboard.attention.degraded")} />,
    );
  }
  if (status?.parityBlocked) {
    attention.push(
      <Banner key="blocked" tone="warning" title={t("dashboard.attention.syncBlocked")} />,
    );
  }
  for (const job of jobs.filter((entry) => entry.status === "failed")) {
    attention.push(
      <Banner
        key={job.id}
        tone="error"
        title={t("dashboard.attention.failedJob", { type: job.type })}
        description={job.error?.message}
      />,
    );
  }

  if (loading) {
    return <LoadingBlock rows={6} />;
  }

  if (error && !pool) {
    return <Banner tone="error" title={error} />;
  }

  if (!pool?.mounted) {
    return (
      <EmptyState
        icon={HardDrive}
        title={t("dashboard.noArray.title")}
        description={t("dashboard.noArray.description")}
      />
    );
  }

  const usedPercent = summary.total > 0 ? Math.round((summary.used / summary.total) * 100) : 0;

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-2xl font-semibold font-heading">{t("nav.dashboard")}</h1>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        <MetricTile
          title={t("dashboard.capacity.title")}
          value={`${formatBytes(summary.used)} / ${formatBytes(summary.total)}`}
          description={t("dashboard.capacity.usedPercent", { percent: usedPercent })}
          progress={usedPercent}
          footer={<StatusBadge tone={arrayStatusTone(status)}>{arrayStatusLabel(status, t)}</StatusBadge>}
        />
        <MetricTile
          title={t("dashboard.parity.title")}
          value={parity.label}
          description={t("dashboard.parity.hint")}
          footer={<StatusBadge tone={parity.tone}>{parity.label}</StatusBadge>}
          to={PATHS.storageParity}
        />
        <MetricTile
          title={t("dashboard.disks.title")}
          value={t("dashboard.disks.summary", {
            data: summary.dataCount,
            parity: summary.parityCount,
            cache: summary.cacheCount,
          })}
          to={PATHS.storageDisks}
        />
      </div>
      <section>
        <h2 className="mb-2 font-medium">{t("dashboard.diskStrip.title")}</h2>
        <ScrollArea className="w-full">
          <div className="flex gap-3 pb-2">
            {(pool?.disks ?? [])
              .filter((disk) => disk.role === "data" || disk.role === "parity" || disk.role === "cache")
              .map((disk) => {
                const size = disk.sizeBytes ?? 0;
                const used = disk.usedBytes ?? 0;
                const percent = size > 0 ? Math.round((used / size) * 100) : 0;
                return (
                  <MetricTile
                    key={disk.device}
                    title={disk.device}
                    value={disk.role}
                    description={formatBytes(used)}
                    progress={percent}
                    to={diskDetailPath(disk.device)}
                  />
                );
              })}
          </div>
        </ScrollArea>
      </section>
      <div className="grid gap-4 lg:grid-cols-2">
        <TimeSeriesChart
          title={t("dashboard.throughput.title")}
          description={t("dashboard.throughput.description")}
          data={null}
        />
        <TimeSeriesChart
          title={t("dashboard.network.title")}
          description={t("dashboard.network.description")}
          data={null}
        />
      </div>
      {activeJobs.length > 0 ? (
        <section className="flex flex-col gap-3">
          <h2 className="font-medium">{t("dashboard.activeJobs.title")}</h2>
          {activeJobs.map((job) => (
            <JobProgress key={job.id} job={job} />
          ))}
        </section>
      ) : null}
      {attention.length > 0 ? (
        <section className="flex flex-col gap-3">
          <h2 className="font-medium">{t("dashboard.attention.title")}</h2>
          {attention}
        </section>
      ) : null}
    </div>
  );
}
