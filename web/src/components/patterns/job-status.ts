import type { useTranslation } from "react-i18next";

import type { StatusTone } from "@/components/patterns/status-badge";
import type { components } from "@/lib/api/client";

type JobStatus = components["schemas"]["Job"]["status"];

export function jobStatusTone(status: JobStatus): StatusTone {
  switch (status) {
    case "succeeded":
      return "success";
    case "failed":
      return "error";
    case "running":
      return "info";
    case "interrupted":
      return "warning";
    case "queued":
    case "cancelled":
      return "outline";
    default:
      return "outline";
  }
}

export function jobStatusLabel(
  status: JobStatus,
  t: ReturnType<typeof useTranslation>["t"],
): string {
  switch (status) {
    case "running":
    case "queued":
    case "interrupted":
    case "failed":
    case "succeeded":
    case "cancelled":
      return t(`jobs.status.${status}`);
    default:
      return t("jobs.status.unknown");
  }
}
