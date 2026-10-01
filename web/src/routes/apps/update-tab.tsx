import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { ConfirmDialog } from "@/components/patterns/confirm";
import { JobProgress } from "@/components/patterns/job-progress";
import { LoadingBlock } from "@/components/patterns/loading";
import { SettingSwitch } from "@/components/patterns/setting-switch";
import { StatusBadge, type StatusTone } from "@/components/patterns/status-badge";
import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { jobDetailPath } from "@/hooks/paths";
import type { components } from "@/lib/api/client";
import {
  getAppUpdateHistory,
  getAppUpdates,
  getJob,
  postAppRevert,
  postAppUpdate,
  putAppUpdatePolicy,
} from "@/lib/api/operations";
import { apiErrorMessage, isAbortError } from "@/lib/api/request";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import type { App, AppUpdate } from "@/routes/apps/containers";

type Job = components["schemas"]["Job"];
type ListAppUpdatesOK = components["schemas"]["ListAppUpdatesOK"];
type ListAppUpdateHistoryOK = components["schemas"]["ListAppUpdateHistoryOK"];
type JobKind = "update" | "revert";

const JOB_POLL_MS = 2000;
const FINISHED: Job["status"][] = ["succeeded", "failed", "cancelled", "interrupted"];

function finished(job: Job): boolean {
  return FINISHED.includes(job.status);
}

type Outcome = { tone: StatusTone; labelKey: string; detailKey: string | null };

// Only update_available reads as an update; skipped, failed and not_checked
// are named as what they are and never as up to date.
function outcomeOf(update: AppUpdate | undefined): Outcome {
  switch (update?.status) {
    case "up_to_date":
      return { tone: "success", labelKey: "apps.update.upToDate", detailKey: null };
    case "update_available":
      return update.kind === "new_build"
        ? { tone: "info", labelKey: "apps.update.newBuild", detailKey: null }
        : { tone: "info", labelKey: "apps.update.newVersion", detailKey: null };
    case "skipped":
      return { tone: "warning", labelKey: "apps.update.skipped", detailKey: "apps.update.skippedDetail" };
    case "failed":
      return { tone: "error", labelKey: "apps.update.failed", detailKey: "apps.update.failedDetail" };
    default:
      return { tone: "outline", labelKey: "apps.update.notChecked", detailKey: "apps.update.notCheckedDetail" };
  }
}

type Followed = { id: string; job: Job | null; error: string | null };

// Follows a queued job until it finishes. A request that fails stops the
// following and is reported, so a job whose state is unknown is never shown
// as finished.
function useJobFollow(queued: Job | null, fallback: string): { job: Job | null; error: string | null } {
  const [followed, setFollowed] = useState<Followed | null>(null);
  const queuedId = queued?.id ?? null;

  useEffect(() => {
    if (queuedId === null) {
      return;
    }
    const controller = new AbortController();
    const { signal } = controller;
    let timer: number | undefined;
    const poll = async (): Promise<void> => {
      try {
        const result = await getJob(queuedId, signal);
        if (signal.aborted) {
          return;
        }
        if (result.error !== undefined || result.response?.ok === false || result.data === undefined) {
          setFollowed({ id: queuedId, job: null, error: apiErrorMessage(result.error, fallback) });
          return;
        }
        setFollowed({ id: queuedId, job: result.data, error: null });
        if (finished(result.data)) {
          return;
        }
      } catch (err: unknown) {
        if (!isAbortError(err, signal)) {
          setFollowed({ id: queuedId, job: null, error: apiErrorMessage(undefined, err instanceof Error ? err.message : fallback) });
        }
        return;
      }
      timer = window.setTimeout(() => void poll(), JOB_POLL_MS);
    };
    timer = window.setTimeout(() => void poll(), JOB_POLL_MS);
    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [queuedId, fallback]);

  const current = followed?.id === queuedId ? followed : null;
  return { job: current?.job ?? queued, error: current?.error ?? null };
}

function Fact({ label, children }: { label: string; children: React.ReactNode }): React.ReactElement {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <dt className="text-muted-foreground text-xs">{label}</dt>
      <dd className="min-w-0 break-words text-sm">{children}</dd>
    </div>
  );
}

export function UpdateTab({ app, onChanged }: { app: App; onChanged: () => void }): React.ReactElement {
  const { t } = useTranslation();
  const [queued, setQueued] = useState<{ job: Job; kind: JobKind } | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [revertOpen, setRevertOpen] = useState(false);
  const settled = useRef<string | null>(null);

  const updatesQuery = useApiQuery<ListAppUpdatesOK>({
    queryKey: "app-updates",
    queryFn: (signal) => getAppUpdates(signal),
    fallbackError: t("apps.update.loadFailed"),
  });
  const historyQuery = useApiQuery<ListAppUpdateHistoryOK>({
    queryKey: "app-update-history",
    queryFn: (signal) => getAppUpdateHistory(signal),
    fallbackError: t("apps.update.historyFailed"),
  });
  const followed = useJobFollow(queued?.job ?? null, t("apps.update.jobFailed"));
  const update = useApiMutation<string, Job>({
    mutationFn: (id) => postAppUpdate(id),
    fallbackError: t("apps.update.updateFailed"),
  });
  const revert = useApiMutation<string, Job>({
    mutationFn: (id) => postAppRevert(id),
    fallbackError: t("apps.update.revertFailed"),
  });
  const policy = useApiMutation<boolean, { container: string; bulkExcluded: boolean }>({
    mutationFn: (bulkExcluded) => putAppUpdatePolicy(app.id, bulkExcluded),
    fallbackError: t("apps.update.policyFailed"),
  });

  const job = followed.job;
  const running = job !== null && !finished(job);
  const refreshUpdates = updatesQuery.refresh;
  const refreshHistory = historyQuery.refresh;

  useEffect(() => {
    if (job !== null && finished(job) && settled.current !== job.id) {
      settled.current = job.id;
      void refreshUpdates();
      void refreshHistory();
      onChanged();
    }
  }, [job, refreshUpdates, refreshHistory, onChanged]);

  async function handleUpdate(): Promise<void> {
    setActionError(null);
    const result = await update.mutate(app.id);
    if (result.ok && result.data !== undefined) {
      setQueued({ job: result.data, kind: "update" });
    } else if (!result.ok && !result.aborted) {
      setActionError(result.error);
    }
  }

  async function handleRevert(): Promise<void> {
    const result = await revert.mutate(app.id);
    if (result.ok && result.data !== undefined) {
      setRevertOpen(false);
      setActionError(null);
      setQueued({ job: result.data, kind: "revert" });
    }
  }

  async function handlePolicy(bulkExcluded: boolean): Promise<void> {
    setActionError(null);
    const result = await policy.mutate(bulkExcluded);
    if (result.ok) {
      await updatesQuery.refresh();
    } else if (!result.aborted) {
      setActionError(result.error);
    }
  }

  const updates = updatesQuery.data;
  const entry = updates?.updates.find((candidate) => candidate.container === app.name);
  const outcome = outcomeOf(entry);
  const tag = app.tag || entry?.tag;
  const record = historyQuery.data?.records.find((candidate) => candidate.container === app.name);

  return (
    <div className="flex flex-col gap-4">
      {actionError !== null ? <Banner tone="error" title={actionError} onDismiss={() => setActionError(null)} /> : null}
      {job !== null && queued !== null ? (
        <Card>
          <CardHeader>
            <CardTitle>{t(`apps.update.job.${queued.kind}`, { name: app.name })}</CardTitle>
          </CardHeader>
          <CardPanel className="flex flex-col gap-3">
            <JobProgress job={job} />
            {job.status === "succeeded" ? <Banner tone="info" title={t(`apps.update.job.${queued.kind}Done`)} /> : null}
            {finished(job) && job.status !== "succeeded" ? (
              <Banner
                tone="error"
                title={t(`apps.update.job.${queued.kind}Failed`)}
                description={job.error?.message}
              />
            ) : null}
            {followed.error !== null ? <Banner tone="error" title={followed.error} /> : null}
            <Link to={jobDetailPath(job.id)} className={buttonVariants({ variant: "outline", className: "self-start" })}>
              {t("apps.update.job.view")}
            </Link>
          </CardPanel>
        </Card>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle>{t("apps.update.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-4">
          {updatesQuery.error ? (
            <Banner
              tone="error"
              title={updatesQuery.error}
              action={
                <Button size="xs" variant="outline" onClick={() => void updatesQuery.refresh()}>
                  {t("apps.installed.retry")}
                </Button>
              }
            />
          ) : null}
          {updates === null && !updatesQuery.error ? <LoadingBlock rows={2} /> : null}
          {updates !== null && !updates.available ? (
            <Banner tone="warning" title={updates.message ?? t("apps.update.loadFailed")} />
          ) : null}
          {updates?.available ? (
            <>
              <dl className="grid gap-4 sm:grid-cols-2">
                <Fact label={t("apps.update.current")}>
                  <span className="break-all">{tag || t("apps.detail.notKnown")}</span>
                </Fact>
                <Fact label={t("apps.update.available")}>
                  {entry?.status === "update_available" && entry.kind === "new_version" ? (
                    <span className="break-all">{entry.availableTag}</span>
                  ) : entry?.status === "update_available" ? (
                    t("apps.update.sameTagRebuilt", { tag })
                  ) : (
                    t("apps.detail.notKnown")
                  )}
                </Fact>
                <Fact label={t("apps.update.status")}>
                  <StatusBadge tone={outcome.tone}>
                    {t(outcome.labelKey, { tag: entry?.availableTag ?? "" })}
                  </StatusBadge>
                </Fact>
                <Fact label={t("apps.update.checkedAt")}>
                  {entry?.checkedAt ? new Date(entry.checkedAt).toLocaleString() : t("apps.update.neverChecked")}
                </Fact>
              </dl>
              {outcome.detailKey !== null ? (
                <p className="text-muted-foreground text-sm">{entry?.message || t(outcome.detailKey)}</p>
              ) : null}
              <div className="flex flex-wrap gap-2">
                <Button
                  disabled={entry?.status === "up_to_date" || running || revert.pending}
                  loading={update.pending}
                  onClick={() => void handleUpdate()}
                >
                  {t("apps.update.action")}
                </Button>
              </div>
              <SettingSwitch
                label={t("apps.update.bulk.label")}
                description={t("apps.update.bulk.description")}
                checked={entry?.bulkExcluded ?? false}
                disabled={entry === undefined || policy.pending}
                onCheckedChange={(checked) => void handlePolicy(checked)}
              />
            </>
          ) : null}
        </CardPanel>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("apps.update.previous.title")}</CardTitle>
        </CardHeader>
        <CardPanel className="flex flex-col gap-3">
          {historyQuery.error ? (
            <Banner
              tone="error"
              title={historyQuery.error}
              action={
                <Button size="xs" variant="outline" onClick={() => void historyQuery.refresh()}>
                  {t("apps.installed.retry")}
                </Button>
              }
            />
          ) : null}
          {historyQuery.data === null && !historyQuery.error ? <LoadingBlock rows={1} /> : null}
          {historyQuery.data !== null && !historyQuery.data.available ? (
            <Banner tone="warning" title={historyQuery.data.message ?? t("apps.update.historyFailed")} />
          ) : null}
          {historyQuery.data?.available && record === undefined ? (
            <p className="text-muted-foreground text-sm">{t("apps.update.previous.none")}</p>
          ) : null}
          {record !== undefined && record.revertedAt ? (
            <p className="text-sm">
              {t("apps.update.previous.reverted", { time: new Date(record.revertedAt).toLocaleString() })}
            </p>
          ) : null}
          {record !== undefined && !record.revertedAt && !record.revertible ? (
            <p className="text-sm">
              {t("apps.update.previous.expired", { time: new Date(record.updatedAt).toLocaleString() })}
            </p>
          ) : null}
          {record !== undefined && !record.revertedAt && record.revertible ? (
            <>
              <p className="text-sm">
                {t("apps.update.previous.kept", {
                  time: new Date(record.updatedAt).toLocaleString(),
                  until: new Date(record.keepUntil).toLocaleString(),
                })}
              </p>
              <div className="flex flex-wrap gap-2">
                <Button
                  variant="outline"
                  disabled={running || update.pending}
                  onClick={() => {
                    revert.reset();
                    setRevertOpen(true);
                  }}
                >
                  {t("apps.update.previous.action")}
                </Button>
              </div>
            </>
          ) : null}
        </CardPanel>
      </Card>

      <ConfirmDialog
        open={revertOpen}
        onOpenChange={(open) => {
          if (!revert.pending) {
            setRevertOpen(open);
          }
        }}
        title={t("apps.update.previous.confirmTitle", { name: app.name })}
        description={t("apps.update.previous.confirmDescription")}
        error={revert.error}
        items={record?.snapshotArchive ? [t("apps.update.previous.confirmData")] : undefined}
        confirmLabel={t("apps.update.previous.action")}
        loading={revert.pending}
        onConfirm={() => void handleRevert()}
      />
    </div>
  );
}
