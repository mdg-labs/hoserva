import { useTranslation } from "react-i18next";

import { Banner } from "@/components/patterns/banner";
import { JobProgress } from "@/components/patterns/job-progress";
import { postJobCancel } from "@/lib/api/operations";
import { useApiMutation } from "@/lib/api/use-api-mutation";
import type { Job } from "@/routes/tools-migrate/report";

export function MigrationJob({ job, onChanged }: { job: Job; onChanged: () => void }): React.ReactElement {
  const { t } = useTranslation();
  const cancel = useApiMutation({ mutationFn: postJobCancel, fallbackError: t("toolsMigrate.cancelFailed") });

  async function handleCancel(jobId: string): Promise<void> {
    const result = await cancel.mutate(jobId);
    if (result.ok) {
      onChanged();
    }
  }

  return (
    <div className="flex flex-col gap-2">
      {cancel.error ? <Banner tone="error" title={cancel.error} /> : null}
      <JobProgress job={job} onCancel={(jobId) => void handleCancel(jobId)} />
    </div>
  );
}

export function FailedJobBanner({ job, title }: { job: Job; title: string }): React.ReactElement {
  const { t } = useTranslation();

  return (
    <Banner
      tone="error"
      title={title}
      description={job.error?.message ?? t(`toolsMigrate.jobEnded.${job.status}`, { defaultValue: job.status })}
    />
  );
}
