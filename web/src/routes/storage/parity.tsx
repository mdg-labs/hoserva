import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { GroupedResults } from "@/components/patterns/grouped-results";
import { InlineNote } from "@/components/patterns/inline-note";
import { jobStatusLabel, jobStatusTone } from "@/components/patterns/job-status";
import { LoadingBlock } from "@/components/patterns/loading";
import {
  parityDiffGroupsFromAPI,
  type ParityDiffGroup,
} from "@/components/patterns/parity-diff";
import { StatusBadge } from "@/components/patterns/status-badge";
import { TypedConfirm } from "@/components/patterns/typed-confirm";
import { FormOverlay } from "@/components/patterns/form-overlay";
import { Wizard } from "@/components/patterns/wizard";
import { useUnsavedGuard } from "@/hooks/use-unsaved-guard";
import { useSystemData } from "@/hooks/use-system-status";
import {
  getJob,
  getParity,
  postParityDiff,
  postParityFix,
  postParityScrub,
  postParitySync,
} from "@/lib/api/operations";
import { apiErrorMessage, isAbortError } from "@/lib/api/request";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import { jobTypeLabel } from "@/lib/job-labels";
import type { components } from "@/lib/api/client";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "@/components/ui/dialog";

type ParitySnapshot = components["schemas"]["ParitySnapshot"];

const SYNC_POLL_MS = 2000;
const GUARD_BLOCKED_MARKER = "threshold guard blocked the sync";

// A sync the guard holds is queued like any other and then fails with the
// engine's GuardBlockedError text, so that text is the only signal the
// failed job carries.
function guardBlockDetail(message: string | undefined): string | null {
  const at = message?.indexOf(GUARD_BLOCKED_MARKER) ?? -1;
  if (message === undefined || at < 0) {
    return null;
  }
  return message.slice(at + GUARD_BLOCKED_MARKER.length).replace(/^:\s*/, "");
}

type SyncOutcome =
  | { kind: "succeeded" }
  | { kind: "blocked"; detail: string }
  | { kind: "failed"; message: string }
  | { kind: "stopped" }
  | { kind: "aborted" };

function pause(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = window.setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        window.clearTimeout(timer);
        resolve();
      },
      { once: true },
    );
  });
}

// Polls a sync job until it ends. A job the guard refused is reported as
// "blocked" with the engine's own text, never as a failure to retry.
async function followSyncJob(jobId: string, signal: AbortSignal): Promise<SyncOutcome> {
  for (;;) {
    try {
      const result = await getJob(jobId, signal);
      if (signal.aborted) {
        return { kind: "aborted" };
      }
      if (result.error !== undefined || result.response?.ok === false || result.data === undefined) {
        return { kind: "failed", message: apiErrorMessage(result.error) };
      }
      const job = result.data;
      if (job.status === "succeeded") {
        return { kind: "succeeded" };
      }
      if (job.status === "failed") {
        const detail = guardBlockDetail(job.error?.message);
        return detail !== null
          ? { kind: "blocked", detail }
          : { kind: "failed", message: apiErrorMessage(job.error ?? undefined) };
      }
      if (job.status === "cancelled" || job.status === "interrupted") {
        return { kind: "stopped" };
      }
    } catch (err: unknown) {
      if (isAbortError(err, signal)) {
        return { kind: "aborted" };
      }
      return { kind: "failed", message: apiErrorMessage(undefined, err instanceof Error ? err.message : undefined) };
    }
    await pause(SYNC_POLL_MS, signal);
    if (signal.aborted) {
      return { kind: "aborted" };
    }
  }
}

function freshnessTone(freshness: ParitySnapshot["freshness"]): "success" | "warning" | "error" {
  switch (freshness) {
    case "red":
      return "error";
    case "amber":
      return "warning";
    default:
      return "success";
  }
}

export function ParityPage(): React.ReactElement {
  const { t } = useTranslation();
  const { status, pool, doctor, jobs, loading, error, refresh } = useSystemData();
  const parityQuery = useApiQuery<ParitySnapshot>({
    queryKey: "parity-snapshot",
    queryFn: (signal) => getParity(signal),
  });
  const diffMutation = useApiMutation({ mutationFn: () => postParityDiff() });
  const syncMutation = useApiMutation({ mutationFn: (overrideGuard: boolean) => postParitySync(overrideGuard) });
  const fixMutation = useApiMutation({ mutationFn: () => postParityFix() });
  const scrubMutation = useApiMutation({ mutationFn: () => postParityScrub() });

  const parity = parityQuery.data;
  const parityError = parityQuery.error;
  const parityDiffGroups = useMemo(
    () => (parity?.groups?.length ? parityDiffGroupsFromAPI(parity.groups) : []),
    [parity],
  );
  const [localDiffGroups, setLocalDiffGroups] = useState<ParityDiffGroup[] | null>(null);
  const diffGroups = localDiffGroups ?? parityDiffGroups;
  const [runDiffOpen, setRunDiffOpen] = useState(false);
  const [syncOpen, setSyncOpen] = useState(false);
  const [fixOpen, setFixOpen] = useState(false);
  const [fixStep, setFixStep] = useState(0);
  const [fixDirty, setFixDirty] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [diffDialogError, setDiffDialogError] = useState<string | null>(null);
  const [syncDialogError, setSyncDialogError] = useState<string | null>(null);
  const [syncJobId, setSyncJobId] = useState<string | null>(null);
  const [syncRunning, setSyncRunning] = useState(false);
  const [syncFinished, setSyncFinished] = useState(false);
  const [syncBlockDetail, setSyncBlockDetail] = useState<string | null>(null);
  const [overrideOpen, setOverrideOpen] = useState(false);
  const [overrideValue, setOverrideValue] = useState("");
  const [overrideError, setOverrideError] = useState<string | null>(null);
  const [overrideChecking, setOverrideChecking] = useState(false);
  const [overrideBlock, setOverrideBlock] = useState<string | null>(null);
  const [overrideNotice, setOverrideNotice] = useState<string | null>(null);
  const overrideRun = useRef<AbortController | null>(null);
  const overrideJobId = useRef<string | null>(null);
  const refreshAfterSync = useRef(async (): Promise<void> => {});
  const { requestClose, guardDialog } = useUnsavedGuard({
    dirty: fixDirty,
    onClose: () => {
      setFixOpen(false);
      setFixStep(0);
      setFixDirty(false);
    },
  });

  const parityDisk = pool?.disks.find((disk) => disk.role === "parity");
  const guard = parity?.guard;
  const guardTripped = (guard?.wouldBlock ?? Boolean(status?.parityBlocked)) || syncBlockDetail !== null;
  const guardSummary =
    syncBlockDetail !== null
      ? t("parity.guard.syncBlocked", { detail: syncBlockDetail })
      : guard?.summary;
  const parityJobs = jobs.filter((job) => job.class === "parity");

  const refreshParity = parityQuery.refresh;
  useEffect(() => {
    refreshAfterSync.current = async () => {
      await refresh();
      await refreshParity();
    };
  }, [refresh, refreshParity]);

  // The block an override may approve belongs to one overlay session. Any
  // other sync, a diff, closing the overlay or leaving the page ends it.
  const discardOverride = useCallback((): void => {
    overrideRun.current?.abort();
    overrideRun.current = null;
    overrideJobId.current = null;
    setOverrideOpen(false);
    setOverrideValue("");
    setOverrideError(null);
    setOverrideChecking(false);
    setOverrideBlock(null);
    setOverrideNotice(null);
  }, []);

  useEffect(() => discardOverride, [discardOverride]);

  useEffect(() => {
    if (syncJobId === null) {
      return;
    }
    const controller = new AbortController();
    const settle = (): void => {
      setSyncRunning(false);
      void refreshAfterSync.current();
    };
    void followSyncJob(syncJobId, controller.signal).then((outcome) => {
      switch (outcome.kind) {
        case "aborted":
          return;
        case "succeeded":
          setOverrideBlock(null);
          setSyncFinished(true);
          break;
        case "blocked":
          setOverrideBlock(null);
          setSyncBlockDetail(outcome.detail);
          break;
        case "failed":
          setActionError(outcome.message);
          break;
        case "stopped":
          break;
      }
      settle();
    });
    return () => controller.abort();
  }, [syncJobId]);

  const handleRunDiff = async (): Promise<void> => {
    const result = await diffMutation.mutate(undefined);
    if (!result.ok) {
      setDiffDialogError(result.error);
      return;
    }
    setDiffDialogError(null);
    setRunDiffOpen(false);
    setSyncBlockDetail(null);
    discardOverride();
    if (result.data) {
      setLocalDiffGroups(parityDiffGroupsFromAPI(result.data.groups));
      await parityQuery.refresh();
    }
  };

  const followSync = (jobId: string): void => {
    setSyncFinished(false);
    setSyncRunning(true);
    setSyncJobId(jobId);
  };

  const closeOverride = (): void => {
    const startedJob = overrideJobId.current;
    discardOverride();
    if (startedJob !== null) {
      followSync(startedJob);
    }
  };

  const handleSync = async (): Promise<void> => {
    const result = await syncMutation.mutate(false);
    if (!result.ok) {
      if (!result.aborted) {
        setSyncDialogError(result.error);
      }
      return;
    }
    setSyncDialogError(null);
    setSyncOpen(false);
    discardOverride();
    setActionError(null);
    setSyncBlockDetail(null);
    if (result.data) {
      followSync(result.data.id);
    }
    await refresh();
    await parityQuery.refresh();
  };

  // Opening the override asks the guard again with confirm: false. Only a
  // refusal that request returns can be confirmed, in this overlay session.
  const handleOpenOverride = async (): Promise<void> => {
    closeOverride();
    const controller = new AbortController();
    overrideRun.current = controller;
    setOverrideOpen(true);
    setOverrideChecking(true);
    const result = await syncMutation.mutate(false);
    if (controller.signal.aborted) {
      if (result.ok && result.data) {
        followSync(result.data.id);
      }
      return;
    }
    if (!result.ok || !result.data) {
      setOverrideChecking(false);
      if (!result.ok && !result.aborted) {
        setOverrideError(result.error);
      }
      return;
    }
    overrideJobId.current = result.data.id;
    const outcome = await followSyncJob(result.data.id, controller.signal);
    if (outcome.kind === "aborted") {
      return;
    }
    overrideJobId.current = null;
    setOverrideChecking(false);
    switch (outcome.kind) {
      case "blocked":
        setOverrideBlock(outcome.detail);
        setSyncBlockDetail(outcome.detail);
        setSyncFinished(false);
        break;
      case "succeeded":
        setOverrideNotice(t("parity.override.notRefused"));
        setSyncBlockDetail(null);
        setSyncFinished(true);
        break;
      case "failed":
        setOverrideError(outcome.message);
        break;
      case "stopped":
        setOverrideError(t("parity.override.stopped"));
        break;
    }
    await refreshAfterSync.current();
  };

  const handleOverrideConfirm = async (): Promise<void> => {
    const controller = overrideRun.current;
    if (overrideBlock === null || controller === null || controller.signal.aborted) {
      return;
    }
    const result = await syncMutation.mutate(true);
    if (controller.signal.aborted) {
      if (result.ok && result.data) {
        followSync(result.data.id);
      }
      return;
    }
    if (!result.ok) {
      setOverrideBlock(null);
      setOverrideValue("");
      if (!result.aborted) {
        setOverrideError(result.error);
      }
      return;
    }
    discardOverride();
    setActionError(null);
    setSyncBlockDetail(null);
    if (result.data) {
      followSync(result.data.id);
    }
    await refresh();
    await parityQuery.refresh();
  };

  const handleFixStart = async (): Promise<void> => {
    const result = await fixMutation.mutate(undefined);
    if (!result.ok) {
      setActionError(result.error);
      return;
    }
    setActionError(null);
    setFixOpen(false);
    setFixStep(0);
    setFixDirty(false);
    await refresh();
  };

  const handleScrub = async (): Promise<void> => {
    const result = await scrubMutation.mutate(undefined);
    if (!result.ok) {
      setActionError(result.error);
      return;
    }
    setActionError(null);
    await refresh();
  };

  if (loading || parityQuery.loading) {
    return <LoadingBlock />;
  }

  const freshnessMessage =
    doctor?.checks.find((check) => check.id === "parity_freshness")?.message ?? "—";

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-2xl font-semibold font-heading">{t("storageNav.parity")}</h1>
        <p className="text-muted-foreground">{t("parity.description")}</p>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      {parityError ? <Banner tone="error" title={parityError} /> : null}
      {actionError ? <Banner tone="error" title={actionError} /> : null}
      {syncRunning ? (
        <Banner tone="info" title={t("parity.sync.running")} description={t("parity.sync.runningDescription")} />
      ) : null}
      {syncFinished ? <Banner tone="info" title={t("parity.sync.finished")} /> : null}
      <Card>
        <CardHeader>
          <CardTitle>{t("parity.status.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="grid gap-2 text-sm sm:grid-cols-2">
          <p>{t("parity.status.disk", { device: parityDisk?.device ?? "—" })}</p>
          <p>{freshnessMessage}</p>
          <StatusBadge tone={parity ? freshnessTone(parity.freshness) : "outline"}>
            {freshnessMessage}
          </StatusBadge>
          <StatusBadge tone={guardTripped ? "error" : "success"}>
            {guardTripped ? t("parity.guard.blocked") : t("parity.guard.clear")}
          </StatusBadge>
        </CardPanel>
      </Card>
      {guardTripped ? (
        <Banner
          tone="error"
          title={t("parity.guard.bannerTitle")}
          description={guardSummary ?? t("parity.guard.bannerDescription")}
          action={
            <Button size="xs" variant="destructive-outline" onClick={() => void handleOpenOverride()}>
              {t("parity.override.open")}
            </Button>
          }
        />
      ) : null}
      <section className="flex flex-col gap-3">
        <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <h2 className="font-medium">{t("parity.diff.title")}</h2>
            <p className="text-muted-foreground text-sm">{t("parity.diff.runDiffWarning")}</p>
          </div>
          <Button variant="outline" onClick={() => setRunDiffOpen(true)}>
            {t("parity.diff.runDiff")}
          </Button>
        </div>
        {diffGroups.length === 0 ? (
          <InlineNote description={t("parity.diff.empty")} />
        ) : (
          <GroupedResults
            groups={diffGroups.map((group) => ({
              id: group.category,
              label: t(`parity.diff.categories.${group.category}`),
              count: group.count,
              items: group.paths.length > 0 ? group.paths : ["—"],
              defaultOpen: group.category === "removed",
            }))}
          />
        )}
      </section>
      <div className="flex flex-wrap gap-2">
        <Button onClick={() => setSyncOpen(true)}>{t("parity.actions.sync")}</Button>
        <Button variant="outline" onClick={() => void handleScrub()}>
          {t("parity.actions.scrub")}
        </Button>
        <Button variant="outline" onClick={() => setFixOpen(true)}>
          {t("parity.actions.fix")}
        </Button>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>{t("parity.history.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-2 text-sm">
          {parityJobs.length === 0 ? (
            <p className="text-muted-foreground">{t("parity.history.empty")}</p>
          ) : (
            parityJobs.map((job) => (
              <div key={job.id} data-job-id={job.id} className="flex items-center justify-between gap-2">
                <span>{jobTypeLabel(job.type, t)}</span>
                <StatusBadge tone={jobStatusTone(job.status)}>{jobStatusLabel(job.status, t)}</StatusBadge>
              </div>
            ))
          )}
        </CardPanel>
      </Card>
      <ConfirmDialog
        open={runDiffOpen}
        onOpenChange={(open) => {
          setRunDiffOpen(open);
          if (!open) {
            setDiffDialogError(null);
          }
        }}
        title={t("parity.diff.runDiff")}
        description={
          <>
            {t("parity.diff.runDiffWarning")}
            {diffDialogError ? <Banner tone="error" title={diffDialogError} /> : null}
          </>
        }
        confirmLabel={t("parity.diff.runDiffConfirm")}
        loading={diffMutation.pending}
        onConfirm={() => void handleRunDiff()}
      />
      <ConfirmDialog
        open={syncOpen}
        onOpenChange={(open) => {
          setSyncOpen(open);
          if (!open) {
            setSyncDialogError(null);
          }
        }}
        title={t("parity.actions.sync")}
        description={
          <>
            {t("parity.actions.syncDescription")}
            {guardTripped ? (
              <InlineNote
                title={t("parity.actions.guardInfoTitle")}
                description={guardSummary ?? t("parity.guard.bannerDescription")}
              />
            ) : null}
            {syncDialogError ? <Banner tone="error" title={syncDialogError} /> : null}
          </>
        }
        confirmLabel={t("parity.actions.sync")}
        loading={syncMutation.pending}
        onConfirm={() => void handleSync()}
      />
      <FormOverlay
        open={overrideOpen}
        onOpenChange={(open) => {
          if (!open) {
            closeOverride();
          }
        }}
        title={t("parity.override.title")}
        description={t("parity.override.description")}
        footer={
          overrideBlock !== null ? (
            <Button
              variant="destructive"
              disabled={syncMutation.pending || overrideValue !== t("parity.override.phrase")}
              onClick={() => void handleOverrideConfirm()}
            >
              {t("parity.actions.syncAnyway")}
            </Button>
          ) : undefined
        }
      >
        {overrideChecking ? <InlineNote description={t("parity.override.checking")} /> : null}
        {overrideNotice ? <Banner tone="info" title={overrideNotice} /> : null}
        {overrideError ? <Banner tone="error" title={overrideError} /> : null}
        {overrideBlock !== null ? (
          <TypedConfirm
            phrase={t("parity.override.phrase")}
            value={overrideValue}
            onChange={setOverrideValue}
            title={t("parity.override.confirmTitle")}
            items={[
              t("parity.guard.syncBlocked", { detail: overrideBlock }),
              t("parity.override.consequence"),
            ]}
          />
        ) : null}
      </FormOverlay>
      <Dialog open={fixOpen} onOpenChange={(open) => !open && requestClose()}>
        <DialogPopup className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>{t("parity.fix.title")}</DialogTitle>
          </DialogHeader>
          <DialogPanel>
            <Wizard
              step={fixStep}
              stepCount={4}
              title={t(`parity.fix.steps.${fixStep}.title`)}
              description={t(`parity.fix.steps.${fixStep}.description`)}
              onBack={fixStep > 0 ? () => setFixStep((step) => step - 1) : undefined}
              onNext={() => {
                if (fixStep >= 3) {
                  void handleFixStart();
                  return;
                }
                setFixStep((step) => step + 1);
              }}
              nextLabel={fixStep >= 3 ? t("parity.fix.execute") : undefined}
            >
              {fixStep === 2 ? (
                <Banner tone="warning" title={t("parity.fix.unrecoverableTitle")} description={t("parity.fix.unrecoverableDescription")} />
              ) : null}
            </Wizard>
          </DialogPanel>
        </DialogPopup>
      </Dialog>
      {guardDialog}
    </div>
  );
}
