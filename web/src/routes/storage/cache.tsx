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
import type { components } from "@/lib/api/client";
import { getShares, patchShare, postMoverRun, postShareRelocate } from "@/lib/api/operations";
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

function formatJobTiming(
  startedAt: string | null | undefined,
  finishedAt: string | null | undefined,
  t: (key: string, options?: Record<string, unknown>) => string,
): string {
  if (!startedAt) {
    return t("cache.lastRun.notStarted");
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
  const duration =
    minutes > 0
      ? t("cache.lastRun.durationMinutes", { minutes, seconds: remainder })
      : t("cache.lastRun.durationSeconds", { seconds });
  return t("cache.lastRun.startedWithDuration", { start, duration });
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
    queryFn: (signal) => getShares(signal),
  });
  const moverMutation = useApiMutation({
    mutationFn: postMoverRun,
  });
  const cacheModeMutation = useApiMutation<{ shareName: string; cacheMode: ShareCacheMode }, Share>({
    mutationFn: async ({ shareName, cacheMode }) => {
      const result = await patchShare(shareName, { cacheMode });
      if (result.error) {
        return { ...result, error: { ...result.error, message: shareMutationError(result.error, t) } };
      }
      return result;
    },
  });
  const relocateMutation = useApiMutation<{ shareName: string; direction: "cache" | "array" }, components["schemas"]["Job"] | undefined>({
    mutationFn: async ({ shareName, direction }) => {
      const result = await postShareRelocate(shareName, direction);
      if (result.error) {
        return { ...result, error: { ...result.error, message: shareMutationError(result.error, t) } };
      }
      return result;
    },
  });

  const [modeDrafts, setModeDrafts] = useState<Record<string, ShareCacheMode>>({});
  const [pendingShare, setPendingShare] = useState<Share | null>(null);
  const [pendingMode, setPendingMode] = useState<ShareCacheMode | null>(null);
  const [modeDialogOpen, setModeDialogOpen] = useState(false);
  const [modeDialogError, setModeDialogError] = useState<string | null>(null);
  const [moverError, setMoverError] = useState<string | null>(null);
  // Set at each handler's entry and cleared in `finally` so the dialog
  // stays busy across every awaited step, not just the mutations' own
  // `pending` flags — those go back to false while `sharesQuery.refresh()`
  // or the top-level `refresh()` between the patch and the relocate call is
  // still in flight, which would let Cancel close the dialog mid-handler
  // and still let a queued relocate go through (issue #271 finding).
  const [modeDialogHandlerBusy, setModeDialogHandlerBusy] = useState(false);
  const modeDialogBusy = modeDialogHandlerBusy || cacheModeMutation.pending || relocateMutation.pending;

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
    const result = await cacheModeMutation.mutate({ shareName, cacheMode });
    if (!result.ok) {
      if (!result.aborted) setModeDialogError(result.error);
      return false;
    }
    await sharesQuery.refresh();
    setModeDrafts((current) => {
      const next = { ...current };
      delete next[shareName];
      return next;
    });
    return true;
  }

  async function handleSaveModeOnly(): Promise<void> {
    if (!pendingShare || !pendingMode) {
      return;
    }
    setModeDialogHandlerBusy(true);
    setModeDialogError(null);
    try {
      const saved = await saveShareCacheMode(pendingShare.name, pendingMode);
      if (saved) {
        setModeDialogOpen(false);
        setPendingShare(null);
        setPendingMode(null);
      }
    } finally {
      setModeDialogHandlerBusy(false);
    }
  }

  async function handleSaveModeAndRelocate(): Promise<void> {
    if (!pendingShare || !pendingMode || !relocationDirection) {
      return;
    }
    setModeDialogHandlerBusy(true);
    setModeDialogError(null);
    try {
      const saved = await saveShareCacheMode(pendingShare.name, pendingMode);
      if (!saved) {
        return;
      }
      const relocateResult = await relocateMutation.mutate({ shareName: pendingShare.name, direction: relocationDirection });
      if (!relocateResult.ok) {
        if (!relocateResult.aborted) setModeDialogError(relocateResult.error);
        return;
      }
      const relocatedJob = relocateResult.data;
      if (relocatedJob) {
        toastManager.add({
          type: "success",
          title: t("cache.relocation.queuedTitle"),
          description: t("cache.relocation.queuedDescription"),
          actionProps: {
            children: t("cache.relocation.viewJob"),
            onClick: () => navigate(jobDetailPath(relocatedJob.id)),
          },
        });
        await refresh();
      }
      setModeDialogOpen(false);
      setPendingShare(null);
      setPendingMode(null);
    } finally {
      setModeDialogHandlerBusy(false);
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
                  timing: formatJobTiming(lastMoverJob.startedAt, lastMoverJob.finishedAt, t),
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
          // Same rule as the disabled Cancel button: Escape and a backdrop
          // click both come through here as onOpenChange(false), so a busy
          // handler must ignore them too, not just the button (#375).
          if (!open && modeDialogBusy) {
            return;
          }
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
