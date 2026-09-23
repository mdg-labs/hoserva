import { ListChecks } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { Banner } from "@/components/patterns/banner";
import { DataTable, type DataTableColumn } from "@/components/patterns/data-table";
import { EmptyState } from "@/components/patterns/empty-state";
import { JobProgress } from "@/components/patterns/job-progress";
import { LoadingBlock } from "@/components/patterns/loading";
import { StatusBadge } from "@/components/patterns/status-badge";
import { SelectFilter, TableFilters } from "@/components/patterns/table-filters";
import { Button } from "@/components/ui/button";
import {
  JOB_FILTER_ALL,
  JOB_STATUS_FILTER_VALUES,
  JOB_TYPE_FILTER_VALUES,
} from "@/hooks/job-filter-options";
import { jobDetailPath } from "@/hooks/paths";
import { getJobs, postJobCancel } from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import { useApiQuery } from "@/lib/api/use-api-query";
import type { components } from "@/lib/api/client";

type Job = components["schemas"]["Job"];

export function JobsPage(): React.ReactElement {
  const { t } = useTranslation();
  const [statusFilter, setStatusFilter] = useState(JOB_FILTER_ALL);
  const [typeFilter, setTypeFilter] = useState(JOB_FILTER_ALL);

  const jobsQuery = useApiQuery<{ jobs: Job[] }>({
    queryKey: "jobs-list",
    queryFn: (signal) => getJobs(undefined, signal),
    pollIntervalMs: 15_000,
  });
  const cancelMutation = useApiMutation({
    mutationFn: (jobId: string) => postJobCancel(jobId),
  });

  const jobs = jobsQuery.data?.jobs ?? null;
  const error = jobsQuery.error ?? cancelMutation.error;

  const rows = useMemo(() => {
    if (!jobs) return [];
    return jobs.filter((job) => {
      if (statusFilter !== JOB_FILTER_ALL && job.status !== statusFilter) return false;
      if (typeFilter !== JOB_FILTER_ALL && job.type !== typeFilter) return false;
      return true;
    });
  }, [jobs, statusFilter, typeFilter]);

  const handleCancel = async (jobId: string): Promise<void> => {
    const result = await cancelMutation.mutate(jobId);
    if (result.ok) {
      await jobsQuery.refresh();
    }
  };

  const columns: DataTableColumn<Job>[] = [
    {
      id: "type",
      header: t("jobs.columns.type"),
      cell: (job) => <Link to={jobDetailPath(job.id)}>{job.type}</Link>,
    },
    { id: "class", header: t("jobs.columns.class"), cell: (job) => job.class },
    {
      id: "status",
      header: t("jobs.columns.status"),
      cell: (job) => (
        <StatusBadge
          tone={
            job.status === "failed"
              ? "error"
              : job.status === "running"
                ? "info"
                : job.status === "succeeded"
                  ? "success"
                  : "outline"
          }
        >
          {job.status}
        </StatusBadge>
      ),
    },
    {
      id: "progress",
      header: t("jobs.columns.progress"),
      cell: (job) =>
        job.status === "running" || job.status === "interrupted" ? (
          <JobProgress job={job} onCancel={job.cancellable ? handleCancel : undefined} />
        ) : (
          "—"
        ),
    },
    { id: "created", header: t("jobs.columns.created"), cell: (job) => new Date(job.createdAt).toLocaleString() },
  ];

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-2xl font-semibold font-heading">{t("jobProgress.pageTitle")}</h1>
        <p className="text-muted-foreground">{t("jobProgress.pageDescription")}</p>
      </div>
      {error ? <Banner tone="error" title={error} /> : null}
      <TableFilters
        filters={
          <div className="flex flex-wrap gap-2">
            <SelectFilter
              value={statusFilter}
              onChange={setStatusFilter}
              placeholder={t("jobs.filters.status")}
              options={[
                { value: JOB_FILTER_ALL, label: t("jobs.filters.all") },
                ...JOB_STATUS_FILTER_VALUES.map((value) => ({
                  value,
                  label: t(`jobs.status.${value}`),
                })),
              ]}
            />
            <SelectFilter
              value={typeFilter}
              onChange={setTypeFilter}
              placeholder={t("jobs.filters.type")}
              options={[
                { value: JOB_FILTER_ALL, label: t("jobs.filters.all") },
                ...JOB_TYPE_FILTER_VALUES.map((value) => ({
                  value,
                  label: t(`jobs.types.${value}`),
                })),
              ]}
            />
          </div>
        }
      />
      {jobsQuery.loading && !jobs ? <LoadingBlock /> : null}
      {jobs && rows.length === 0 ? (
        <EmptyState
          icon={ListChecks}
          title={t("jobProgress.noJobs")}
          description={t("jobProgress.noJobsDescription")}
        />
      ) : null}
      {rows.length > 0 ? <DataTable columns={columns} rows={rows} getRowKey={(job) => job.id} /> : null}
      <Button variant="outline" onClick={() => void jobsQuery.refresh()}>{t("jobs.refresh")}</Button>
    </div>
  );
}
