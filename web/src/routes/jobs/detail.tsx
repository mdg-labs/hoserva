import { useEffect, useState } from "react";
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
import { hoservaClient, type components } from "@/lib/api/client";

type Job = components["schemas"]["Job"];

export function JobDetailPage(): React.ReactElement {
  const { t } = useTranslation();
  const { jobId = "" } = useParams();
  const [job, setJob] = useState<Job | null>(null);
  const [log, setLog] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    Promise.all([
      hoservaClient.GET("/jobs/{jobId}", { params: { path: { jobId } }, signal: controller.signal }),
      hoservaClient.GET("/jobs/{jobId}/log", { params: { path: { jobId } }, signal: controller.signal }),
    ])
      .then(([jobResult, logResult]) => {
        if (jobResult.error) {
          setError(jobResult.error.message);
          return;
        }
        setJob(jobResult.data ?? null);
        if (typeof logResult.data === "string") {
          setLog(logResult.data);
        }
      })
      .catch((err: unknown) => {
        if (!controller.signal.aborted) {
          setError(err instanceof Error ? err.message : String(err));
        }
      });
    return () => controller.abort();
  }, [jobId]);

  const handleCancel = async (): Promise<void> => {
    await hoservaClient.POST("/jobs/{jobId}/cancel", { params: { path: { jobId } } });
    const { data } = await hoservaClient.GET("/jobs/{jobId}", { params: { path: { jobId } } });
    setJob(data ?? null);
  };

  if (!job && !error) {
    return <LoadingBlock />;
  }

  if (!job) {
    return <Banner tone="error" title={t("jobs.detail.notFound")} />;
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
