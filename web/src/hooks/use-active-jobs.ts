import type { components } from "@/lib/api/client";

type Job = components["schemas"]["Job"];

export function useActiveJobs(jobs: Job[]): Job[] {
  return jobs.filter((job) => job.status === "running" || job.status === "queued");
}
