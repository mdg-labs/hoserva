// /jobs (doc 03 §"Jobs"), scoped down to what #21 needs to demonstrate:
// job-progress against the mock's own fixtures, through the generated API
// client only (D18) — the full jobs page (filters, job detail) is a later
// issue.
import { ListChecks } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { EmptyState } from "@/components/patterns/empty-state";
import { JobProgress } from "@/components/patterns/job-progress";
import { LoadingBlock } from "@/components/patterns/loading";
import { hoservaClient, type components } from "@/lib/api/client";

type Job = components["schemas"]["Job"];

export function JobsPage(): React.ReactElement {
  const { t } = useTranslation();
  const [jobs, setJobs] = useState<Job[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    hoservaClient
      .GET("/jobs", { signal: controller.signal })
      .then(({ data, error: apiError }) => {
        if (apiError) {
          setError(apiError.message);
          return;
        }
        setJobs(data?.jobs ?? []);
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) {
          return;
        }
        setError(err instanceof Error ? err.message : String(err));
      });
    return () => controller.abort();
  }, []);

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-2xl font-semibold font-heading">{t("jobProgress.pageTitle")}</h1>
        <p className="text-muted-foreground">{t("jobProgress.pageDescription")}</p>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      {jobs === null && !error ? <LoadingBlock /> : null}
      {jobs && jobs.length === 0 ? (
        <EmptyState
          icon={ListChecks}
          title={t("jobProgress.noJobs")}
          description={t("jobProgress.noJobsDescription")}
        />
      ) : null}
      {jobs && jobs.length > 0 ? (
        <div className="flex flex-col gap-3">
          {jobs.map((job) => (
            <JobProgress key={job.id} job={job} />
          ))}
        </div>
      ) : null}
    </div>
  );
}
