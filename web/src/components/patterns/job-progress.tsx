// `job-progress` (doc 03 "Shared patterns", `p-progress-2`, `p-tooltip-1`):
// long operations are jobs, shown with progress here rather than a spinner
// in a toast — used in the jobs dropdown, the jobs page and the last step
// of wizards.
import { useTranslation } from "react-i18next";

import { Progress, ProgressLabel, ProgressValue } from "@/components/ui/progress";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import type { components } from "@/lib/api/client";

type Job = components["schemas"]["Job"];

export function JobProgress({
  job,
  onCancel,
}: {
  job: Job;
  onCancel?: (jobId: string) => void;
}): React.ReactElement {
  const { t } = useTranslation();
  const value = job.status === "running" || job.status === "interrupted" ? (job.progress ?? null) : 100;

  return (
    <Progress value={value}>
      <div className="flex items-center justify-between gap-2 text-sm">
        <ProgressLabel>{job.type}</ProgressLabel>
        <div className="flex items-center gap-2">
          <ProgressValue />
          {job.cancellable ? (
            <Button size="xs" variant="ghost" onClick={() => onCancel?.(job.id)}>
              {t("jobProgress.cancel")}
            </Button>
          ) : (
            <Tooltip>
              <TooltipTrigger
                render={
                  <Button size="xs" variant="ghost" disabled aria-label={t("jobProgress.cancelNotAvailable")} />
                }
              >
                {t("jobProgress.cancel")}
              </TooltipTrigger>
              <TooltipPopup>{t("jobProgress.cancelNotAvailable")}</TooltipPopup>
            </Tooltip>
          )}
        </div>
      </div>
    </Progress>
  );
}
