import type { components } from "@/lib/api/client";

export const JOB_FILTER_ALL = "all";

type JobStatus = components["schemas"]["Job"]["status"];

// A Record keyed by every JobStatus forces a compile error here if the
// generated schema ever adds a status this filter doesn't know about.
const JOB_STATUS_FILTER_MEMBERS: Record<JobStatus, true> = {
  running: true,
  queued: true,
  interrupted: true,
  failed: true,
  succeeded: true,
  cancelled: true,
};

export const JOB_STATUS_FILTER_VALUES = Object.keys(JOB_STATUS_FILTER_MEMBERS) as JobStatus[];

export const JOB_TYPE_FILTER_VALUES = ["sync", "scrub", "fix", "mover"] as const;
