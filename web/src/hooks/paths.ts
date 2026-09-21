export const PATHS = {
  dashboard: "/",
  storage: "/storage",
  storageDisks: "/storage/disks",
  storageWakeEvents: "/storage/disks/wake-events",
  storageParity: "/storage/parity",
  jobs: "/jobs",
  shares: "/shares",
  users: "/users",
} as const;

export function diskDetailPath(device: string): string {
  return `${PATHS.storageDisks}/${encodeURIComponent(device)}`;
}

export function jobDetailPath(jobId: string): string {
  return `${PATHS.jobs}/${jobId}`;
}

export function shareDetailPath(name: string): string {
  return `${PATHS.shares}/${encodeURIComponent(name)}`;
}
