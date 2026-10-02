import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { JobProgress } from "@/components/patterns/job-progress";
import { isJobFinished } from "@/components/patterns/job-status";
import { LogView } from "@/components/patterns/log-view";
import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { jobDetailPath } from "@/hooks/paths";
import type { components } from "@/lib/api/client";
import { getJob, getJobLog } from "@/lib/api/operations";
import { apiErrorMessage, isAbortError } from "@/lib/api/request";
import { appComposePath } from "@/routes/apps/containers";
import { portConflict } from "@/routes/apps/install-form";

type Job = components["schemas"]["Job"];
type TemplateInstallResult = components["schemas"]["TemplateInstallResult"];

const JOB_POLL_MS = 2000;
const INSTALLED_PATH = "/apps";

export type StartState =
  | { kind: "starting" }
  | { kind: "notStarted"; message: string }
  | { kind: "queued"; job: Job };

type Followed = { id: string; job: Job | null; log: string | null; error: string | null };

// Follows the start job and its output until the job finishes. A request that
// fails stops the following and says so: a job whose state is unknown is never
// shown as running or finished. An output that cannot be read is reported
// beside the job's state, which it does not hide.
function useJobRun(queued: Job, attempt: number, fallback: string, logFallback: string): Followed | null {
  const [followed, setFollowed] = useState<Followed | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    const { signal } = controller;
    let timer: number | undefined;
    const tick = async (): Promise<void> => {
      try {
        const jobResult = await getJob(queued.id, signal);
        if (signal.aborted) {
          return;
        }
        if (jobResult.error !== undefined || jobResult.response?.ok === false || jobResult.data === undefined) {
          setFollowed((prev) => ({
            id: queued.id,
            job: prev?.job ?? null,
            log: prev?.log ?? null,
            error: apiErrorMessage(jobResult.error, fallback),
          }));
          return;
        }
        const job = jobResult.data;
        const logResult = await getJobLog(queued.id, signal);
        if (signal.aborted) {
          return;
        }
        const logFailed = logResult.error !== undefined || logResult.response?.ok === false;
        setFollowed((prev) => ({
          id: queued.id,
          job,
          log: logFailed ? (prev?.log ?? null) : (logResult.data ?? null),
          error: logFailed ? apiErrorMessage(logResult.error, logFallback) : null,
        }));
        if (isJobFinished(job.status)) {
          return;
        }
      } catch (err: unknown) {
        if (!isAbortError(err, signal)) {
          setFollowed((prev) => ({
            id: queued.id,
            job: prev?.job ?? null,
            log: prev?.log ?? null,
            error: err instanceof Error ? err.message : fallback,
          }));
        }
        return;
      }
      timer = window.setTimeout(() => void tick(), JOB_POLL_MS);
    };
    void tick();
    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [queued.id, attempt, fallback, logFallback]);

  return followed?.id === queued.id ? followed : null;
}

function StartJob({ queued, name, onRetryStart }: { queued: Job; name: string; onRetryStart: () => void }): React.ReactElement {
  const { t } = useTranslation();
  const [attempt, setAttempt] = useState(0);
  const followed = useJobRun(queued, attempt, t("apps.install.run.followFailed"), t("apps.install.run.logFailed"));
  const job = followed?.job ?? queued;
  const finished = isJobFinished(job.status);

  return (
    <div className="flex flex-col gap-4">
      <JobProgress job={job} />
      {job.status === "succeeded" ? (
        <Banner
          tone="info"
          title={t("apps.install.run.running", { name })}
          description={t("apps.install.run.runningHelp")}
        />
      ) : null}
      {finished && job.status !== "succeeded" ? (
        <Banner
          tone="error"
          title={t("apps.install.run.startFailedTitle", { name })}
          description={job.error?.message ?? t("apps.install.run.startFailedHelp")}
          action={
            <Button size="xs" variant="outline" onClick={onRetryStart}>
              {t("apps.install.run.retryStart")}
            </Button>
          }
        />
      ) : null}
      {followed?.error ? (
        <Banner
          tone="error"
          title={followed.error}
          action={
            !finished && followed.job !== null ? (
              <Button size="xs" variant="outline" onClick={() => setAttempt((n) => n + 1)}>
                {t("apps.install.run.followAgain")}
              </Button>
            ) : undefined
          }
        />
      ) : null}
      <LogView content={followed?.log ?? null} followEnd={!finished} />
      <Link to={jobDetailPath(job.id)} className={buttonVariants({ variant: "outline", className: "self-start" })}>
        {t("apps.install.run.viewJob")}
      </Link>
    </div>
  );
}

// What follows a successful install: the app is on disk from here on, so this
// panel replaces the form and no second install is offered, whatever the start
// does. A start that does not happen says so and offers only starting again.
export function InstallRun({
  result,
  start,
  onRetryStart,
}: {
  result: TemplateInstallResult;
  start: StartState;
  onRetryStart: () => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const name = result.stack.name;
  const moved = result.plan.inputs.flatMap((input) => {
    const conflict = portConflict(input);
    return conflict ? [conflict] : [];
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("apps.install.run.title", { name })}</CardTitle>
      </CardHeader>
      <CardPanel className="flex flex-col gap-4">
        <Banner tone="info" title={t("apps.install.run.installed", { name })} description={t("apps.install.run.installedHelp")} />
        {moved.map((conflict) => (
          <Banner
            key={conflict.requested}
            tone="warning"
            title={t("apps.install.run.portMoved", { requested: conflict.requested, used: conflict.suggested })}
          />
        ))}
        {start.kind === "starting" ? (
          <p role="status" className="text-muted-foreground text-sm">
            {t("apps.install.run.starting", { name })}
          </p>
        ) : null}
        {start.kind === "notStarted" ? (
          <Banner
            tone="error"
            title={t("apps.install.run.notStartedTitle", { name })}
            description={start.message}
            action={
              <Button size="xs" variant="outline" onClick={onRetryStart}>
                {t("apps.install.run.retryStart")}
              </Button>
            }
          />
        ) : null}
        {start.kind === "queued" ? <StartJob key={start.job.id} queued={start.job} name={name} onRetryStart={onRetryStart} /> : null}
        <div className="flex flex-wrap gap-2">
          <Link to={INSTALLED_PATH} className={buttonVariants()}>
            {t("apps.install.run.toInstalled")}
          </Link>
          <Link to={appComposePath(name)} className={buttonVariants({ variant: "outline" })}>
            {t("apps.install.run.openCompose")}
          </Link>
        </div>
      </CardPanel>
    </Card>
  );
}
