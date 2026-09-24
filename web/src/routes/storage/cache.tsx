import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { InlineNote } from "@/components/patterns/inline-note";
import { LoadingBlock } from "@/components/patterns/loading";
import { MetricTile } from "@/components/patterns/metric-tile";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "@/components/ui/select";
import { toastManager } from "@/components/ui/toast";
import { jobDetailPath } from "@/hooks/paths";
import { useSystemData } from "@/hooks/use-system-status";
import { hoservaClient, type components } from "@/lib/api/client";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import { shareMutationError, shareRelocationDirection } from "@/routes/shares/cache-mode";
import { formatBytes } from "@/routes/storage-setup/config-preview";

type Share = components["schemas"]["Share"];
type ShareCacheMode = components["schemas"]["ShareCacheMode"];
type Job = components["schemas"]["Job"];

const CACHE_MODES: ShareCacheMode[] = ["cache-then-move", "cache-only", "array-only"];
const SETTINGS_BACKUP_ROUTE = "/settings/backup";
const SETTINGS_SCHEDULES_ROUTE = "/settings/schedules";

function formatJobTiming(startedAt: string | null | undefined, finishedAt: string | null | undefined): string {
  if (!startedAt) {
    return "—";
  }
  const start = new Date(startedAt).toLocaleString();
  if (!finishedAt) {
    return start;
  }
  const durationMs = new Date(finishedAt).getTime() - new Date(startedAt).getTime();
  if (durationMs < 0) {
    return start;
  }
  const seconds = Math.round(durationMs / 1000);
  const minutes = Math.floor(seconds / 60);
  const remainder = seconds % 60;
  const duration = minutes > 0 ? `${minutes}m ${remainder}s` : `${seconds}s`;
  return `${start} · ${duration}`;
}

function jobStatusTone(status: Job["status"]): "success" | "error" | "info" | "outline" {
  switch (status) {
    case "failed":
      return "error";
    case "succeeded":
      return "success";
    case "running":
    case "queued":
    case "interrupted":
      return "info";
    default:
      return "outline";
  }
}

export function CachePage(): React.ReactElement {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { pool, jobs, loading: systemLoading, error: systemError, refresh } = useSystemData();
  const sharesQuery = useApiQuery<{ shares: Share[] }>({
    queryKey: "cache-shares",
    queryFn: (signal) => hoservaClient.GET("/shares", { signal }),
  });
  const moverMutation = useApiMutation({
    mutationFn: () => hoservaClient.POST("/mover/run"),
  });

  const [modeDrafts, setModeDrafts] = useState<Record<string, ShareCacheMode>>({});
  const [pendingShare, setPendingShare] = useState<Share | null>(null);
  const [pendingMode, setPendingMode] = useState<ShareCacheMode | null>(null);
  const [modeDialogOpen, setModeDialogOpen] = useState(false);
  const [modeDialogError, setModeDialogError] = useState<string | null>(null);
  const [modeDialogBusy, setModeDialogBusy] = useState(false);
  const [moverError, setMoverError] = useState<string | null>(null);

  const shares = sharesQuery.data?.shares ?? null;
  const loadError = systemError ?? sharesQuery.error;
  const loading = systemLoading || sharesQuery.loading;

  const cacheDisk = pool?.disks.find((disk) => disk.role === "cache");
  const cacheSize = cacheDisk?.sizeBytes ?? 0;
  const cacheUsed = cacheDisk?.usedBytes;
  const cachePercent = cacheUsed != null && cacheSize > 0 ? Math.round((cacheUsed / cacheSize) * 100) : null;

  const hasCacheOnlyShare = shares?.some((share) => share.cacheMode === "cache-only") ?? false;

  const lastMoverJob = useMemo(() => {
    return jobs
      .filter((job) => job.class === "array_write" && job.type === "mover")
      .sort((a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime())[0] ?? null;
  }, [jobs]);

  const relocationDirection =
    pendingShare && pendingMode ? shareRelocationDirection(pendingShare.cacheMode, pendingMode) : null;

  function openModeDialog(share: Share, nextMode: ShareCacheMode): void {
    if (nextMode === share.cacheMode) {
      return;
    }
    setPendingShare(share);
    setPendingMode(nextMode);
    setModeDialogError(null);
    setModeDialogOpen(true);
  }

  function closeModeDialog(): void {
    setModeDialogOpen(false);
    setPendingShare(null);
    setPendingMode(null);
    setModeDialogError(null);
    if (pendingShare) {
      setModeDrafts((current) => {
        const next = { ...current };
        delete next[pendingShare.name];
        return next;
      });
    }
  }

  async function saveShareCacheMode(shareName: string, cacheMode: ShareCacheMode): Promise<boolean> {
    const { data, error: apiError } = await hoservaClient.PATCH("/shares/{name}", {
      params: { path: { name: shareName } },
      body: { cacheMode },
    });
    if (apiError) {
      setModeDialogError(shareMutationError(apiError, t));
      return false;
    }
    if (data) {
      await sharesQuery.refresh();
      setModeDrafts((current) => {
        const next = { ...current };
        delete next[shareName];
        return next;
      });
    }
    return true;
  }

  async function handleSaveModeOnly(): Promise<void> {
    if (!pendingShare || !pendingMode) {
      return;
    }
    setModeDialogBusy(true);
    setModeDialogError(null);
    try {
      const saved = await saveShareCacheMode(pendingShare.name, pendingMode);
      if (saved) {
        setModeDialogOpen(false);
        setPendingShare(null);
        setPendingMode(null);
      }
    } finally {
      setModeDialogBusy(false);
    }
  }

  async function handleSaveModeAndRelocate(): Promise<void> {
    if (!pendingShare || !pendingMode || !relocationDirection) {
      return;
    }
    setModeDialogBusy(true);
    setModeDialogError(null);
    try {
      const saved = await saveShareCacheMode(pendingShare.name, pendingMode);
      if (!saved) {
        return;
      }
      const { data, error: apiError } = await hoservaClient.POST("/shares/{name}/relocate", {
        params: { path: { name: pendingShare.name } },
        body: { to: relocationDirection },
      });
      if (apiError) {
        setModeDialogError(shareMutationError(apiError, t));
        return;
      }
      if (data) {
        toastManager.add({
          type: "success",
          title: t("cache.relocation.queuedTitle"),
          description: t("cache.relocation.queuedDescription"),
          actionProps: {
            children: t("cache.relocation.viewJob"),
            onClick: () => navigate(jobDetailPath(data.id)),
          },
        });
        await refresh();
      }
      setModeDialogOpen(false);
      setPendingShare(null);
      setPendingMode(null);
    } finally {
      setModeDialogBusy(false);
    }
  }

  async function handleRunMover(): Promise<void> {
    setMoverError(null);
    const result = await moverMutation.mutate(undefined);
    if (!result.ok) {
      setMoverError(result.error);
      return;
    }
    if (result.data) {
      toastManager.add({
        type: "success",
        title: t("cache.mover.queuedTitle"),
        description: t("cache.mover.queuedDescription"),
        actionProps: {
          children: t("cache.mover.viewJob"),
          onClick: () => navigate(jobDetailPath(result.data!.id)),
        },
      });
      await refresh();
    }
  }

  const shareColumns: DataTableColumn<Share>[] = useMemo(
    () => [
      {
        id: "name",
        header: t("cache.shares.columns.name"),
        cell: (share) => share.name,
      },
      {
        id: "cacheMode",
        header: t("cache.shares.columns.cacheMode"),
        cell: (share) => {
          const draft = modeDrafts[share.name] ?? share.cacheMode;
          return (
            <Select
              value={draft}
              onValueChange={(value) => {
                if (!value || value === share.cacheMode) {
                  setModeDrafts((current) => {
                    const next = { ...current };
                    delete next[share.name];
                    return next;
                  });
                  return;
                }
                setModeDrafts((current) => ({ ...current, [share.name]: value as ShareCacheMode }));
                openModeDialog(share, value as ShareCacheMode);
              }}
            >
              <SelectTrigger className="w-full max-w-56" aria-label={t("cache.shares.modeSelect", { name: share.name })}>
                <SelectValue />
              </SelectTrigger>
              <SelectPopup>
                {CACHE_MODES.map((mode) => (
                  <SelectItem key={mode} value={mode}>
                    {t(`shares.cacheModes.${mode}.label`)}
                  </SelectItem>
                ))}
              </SelectPopup>
            </Select>
          );
        },
      },
    ],
    [modeDrafts, t],
  );

  if (loading) {
    return <LoadingBlock />;
  }

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-2xl font-semibold font-heading">{t("storageNav.cache")}</h1>
        <p className="text-muted-foreground">{t("cache.description")}</p>
      </div>
      {loadError ? <Banner tone="error" title={loadError} /> : null}
      {moverError ? <Banner tone="error" title={moverError} /> : null}
      {hasCacheOnlyShare ? (
        <Banner
          tone="warning"
          title={t("cache.parityWarning.title")}
          description={t("cache.parityWarning.description")}
          action={
            <Button variant="outline" size="sm" render={<Link to={SETTINGS_BACKUP_ROUTE} />}>
              {t("cache.parityWarning.action")}
            </Button>
          }
        />
      ) : null}
      <MetricTile
        title={t("cache.disk.title")}
        value={cacheDisk?.device ?? t("cache.disk.none")}
        description={
          cacheDisk
            ? cacheUsed != null
              ? `${formatBytes(cacheUsed)} / ${formatBytes(cacheSize)}`
              : t("cache.disk.usageUnavailable")
            : t("cache.disk.noneDescription")
        }
        progress={cachePercent}
        footer={
          cacheDisk ? (
            <StatusBadge tone={cacheDisk.state === "failed" || cacheDisk.state === "missing" ? "error" : "success"}>
              {t(`pool.diskState.${cacheDisk.state}`)}
            </StatusBadge>
          ) : null
        }
      />
      <Card>
        <CardHeader>
          <CardTitle>{t("cache.shares.title")}</CardTitle>
        </CardHeader>
        <CardPanel>
          {shares && shares.length > 0 ? (
            <DataTable columns={shareColumns} rows={shares} getRowKey={(share) => share.name} />
          ) : (
            <InlineNote description={t("cache.shares.empty")} />
          )}
        </CardPanel>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>{t("cache.schedule.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-3 text-sm">
          <p className="text-muted-foreground">{t("cache.schedule.description")}</p>
          <Button variant="outline" size="sm" className="self-start" render={<Link to={SETTINGS_SCHEDULES_ROUTE} />}>
            {t("cache.schedule.link")}
          </Button>
        </CardPanel>
      </Card>
      <div className="flex flex-wrap items-center gap-2">
        <Button loading={moverMutation.pending} onClick={() => void handleRunMover()}>
          {t("cache.mover.run")}
        </Button>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>{t("cache.lastRun.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-2 text-sm">
          {lastMoverJob ? (
            <>
              <div className="flex flex-wrap items-center justify-between gap-2">
                <Link to={jobDetailPath(lastMoverJob.id)} className="font-medium hover:underline">
                  {t("jobs.types.mover")}
                </Link>
                <StatusBadge tone={jobStatusTone(lastMoverJob.status)}>
                  {t(`jobs.status.${lastMoverJob.status}`)}
                </StatusBadge>
              </div>
              <p className="text-muted-foreground">
                {t("cache.lastRun.timing", {
                  timing: formatJobTiming(lastMoverJob.startedAt, lastMoverJob.finishedAt),
                })}
              </p>
              <InlineNote description={t("cache.lastRun.statsUnavailable")} />
            </>
          ) : (
            <InlineNote description={t("cache.lastRun.empty")} />
          )}
        </CardPanel>
      </Card>
      <FormOverlay
        open={modeDialogOpen}
        onOpenChange={(open) => {
          if (!open) {
            closeModeDialog();
          }
        }}
        title={t("shares.detail.cache.confirmTitle")}
        description={t("shares.detail.cache.confirmDescription")}
        footer={
          <div className="flex flex-wrap justify-end gap-2">
            <Button variant="outline" disabled={modeDialogBusy} onClick={() => closeModeDialog()}>
              {t("confirm.cancel")}
            </Button>
            <Button loading={modeDialogBusy} onClick={() => void handleSaveModeOnly()}>
              {t("shares.detail.cache.changeModeOnly")}
            </Button>
            {relocationDirection ? (
              <Button loading={modeDialogBusy} onClick={() => void handleSaveModeAndRelocate()}>
                {t("shares.detail.cache.relocateNow")}
              </Button>
            ) : null}
          </div>
        }
      >
        {modeDialogError ? <Banner tone="error" title={modeDialogError} /> : null}
        {pendingShare && pendingMode ? (
          <p className="text-sm text-muted-foreground">
            {t("shares.detail.cache.modeChangeSummary", {
              share: pendingShare.name,
              from: t(`shares.cacheModes.${pendingShare.cacheMode}.label`),
              to: t(`shares.cacheModes.${pendingMode}.label`),
            })}
          </p>
        ) : null}
      </FormOverlay>
    </div>
  );
}
