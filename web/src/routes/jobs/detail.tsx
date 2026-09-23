import { useTranslation } from "react-i18next";
import { Link, useParams } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { JobProgress } from "@/components/patterns/job-progress";
import { LoadingBlock } from "@/components/patterns/loading";
import { LogView } from "@/components/patterns/log-view";
import { StatusBadge } from "@/components/patterns/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardPanel, CardTitle } from "@/components/ui/card";
import { PATHS } from "@/hooks/paths";
import { getJob, getJobLog, postJobCancel } from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import type { components } from "@/lib/api/client";

type Job = components["schemas"]["Job"];

export function JobDetailPage(): React.ReactElement {
  const { t } = useTranslation();
  const { jobId = "" } = useParams();

  const jobQuery = useApiQuery<Job>({
    queryKey: ["job", jobId],
    queryFn: (signal) => getJob(jobId, signal),
    enabled: Boolean(jobId),
  });
  const logQuery = useApiQuery<string>({
    queryKey: ["job-log", jobId],
    queryFn: (signal) => getJobLog(jobId, signal),
    enabled: Boolean(jobId),
  });
  const cancelMutation = useApiMutation({
    mutationFn: () => postJobCancel(jobId),
    fallbackError: t("jobs.detail.refreshFailed"),
  });

  const job = jobQuery.data;
  const log = logQuery.data;
  const error = jobQuery.error ?? logQuery.error ?? cancelMutation.error;

  const handleCancel = async (): Promise<void> => {
    const result = await cancelMutation.mutate(undefined);
    if (result.ok) {
      await jobQuery.refresh();
    }
  };

  if ((jobQuery.loading || logQuery.loading) && !job && !error) {
    return <LoadingBlock />;
  }

  if (!job) {
    return <Banner tone="error" title={error ?? t("jobs.detail.notFound")} />;
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center justify-between gap-2">
        <div>
          <h1 className="text-2xl font-semibold font-heading">{job.type}</h1>
          <p className="text-muted-foreground font-mono text-sm">{job.id}</p>
        </div>
        <Button variant="outline" render={<Link to={PATHS.jobs} />}>{t("jobs.detail.back")}</Button>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      {job.status === "failed" && job.error ? (
        <Banner tone="error" title={job.error.message} description={job.error.code} />
      ) : null}
      <Card>
        <CardHeader>
          <CardTitle>{t("jobs.detail.metadata")}</CardTitle>
        </CardHeader>
        <CardPanel className="grid gap-2 text-sm sm:grid-cols-2">
          <p>{t("jobs.detail.class", { value: job.class })}</p>
          <p>
            {t("jobs.detail.status")}{" "}
            <StatusBadge tone={job.status === "failed" ? "error" : "info"}>{job.status}</StatusBadge>
          </p>
          <p>{t("jobs.detail.created", { value: new Date(job.createdAt).toLocaleString() })}</p>
        </CardPanel>
      </Card>
      <JobProgress job={job} onCancel={job.cancellable ? handleCancel : undefined} />
      <section>
        <h2 className="mb-2 font-medium">{t("jobs.detail.log")}</h2>
        <LogView content={log} />
      </section>
    </div>
  );
}
