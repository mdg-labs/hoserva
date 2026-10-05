import { useTranslation } from "react-i18next";

import { getJobs } from "@/lib/api/operations";
import { useApiQuery, type UseApiQueryResult } from "@/lib/api/use-api-query";
import { jobActive, type Job } from "@/routes/tools-migrate/report";

const JOBS_LIMIT = 50;
const SYNC_JOBS_LIMIT = 20;
const POLL_MS = 2000;

export type MigrationJobs = UseApiQueryResult<{ jobs: Job[] }>;

// The migration's own jobs are topology-class: the import, the verify and the
// point of no return. They are read from the job list, so a page opened in the
// middle of one finds it.
export function useMigrationJobs(enabled: boolean, polling: boolean): MigrationJobs {
  const { t } = useTranslation();

  return useApiQuery({
    queryKey: "migration-jobs",
    queryFn: (signal) => getJobs({ class: "topology", limit: JOBS_LIMIT }, signal),
    enabled,
    pollIntervalMs: polling ? POLL_MS : undefined,
    fallbackError: t("toolsMigrate.jobsLoadFailed"),
  });
}

// The first sync is queued by the point of no return as an ordinary sync job.
// The unprotected window ends at the first sync created at or after that job
// that succeeded, so a later sync repairs a failed first one; until one has
// succeeded, the newest such sync's state is shown. The list is newest first.
export function initialSyncOf(jobs: Job[], parityJob: Job): Job | undefined {
  const since = Date.parse(parityJob.createdAt);
  const syncs = jobs.filter((job) => job.type === "sync" && Date.parse(job.createdAt) >= since);
  return syncs.findLast((job) => job.status === "succeeded") ?? syncs[0];
}

export function useInitialSyncJobs(enabled: boolean, polling: boolean): MigrationJobs {
  const { t } = useTranslation();

  return useApiQuery({
    queryKey: "migration-sync-jobs",
    queryFn: (signal) => getJobs({ class: "parity", limit: SYNC_JOBS_LIMIT }, signal),
    enabled,
    pollIntervalMs: polling ? POLL_MS : undefined,
    fallbackError: t("toolsMigrate.parity.syncLoadFailed"),
  });
}

export function anyActive(...jobs: Array<Job | undefined>): boolean {
  return jobs.some((job) => jobActive(job));
}
