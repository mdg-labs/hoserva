import { hoservaClient, type components } from "@/lib/api/client";

type ArrayDiskFilesystem = components["schemas"]["ArrayDiskFilesystem"];
type CreateArrayRequest = components["schemas"]["CreateArrayRequest"];

export function getStatus(signal?: AbortSignal) {
  return hoservaClient.GET("/status", { signal });
}

export function getPool(signal?: AbortSignal) {
  return hoservaClient.GET("/pool", { signal });
}

export function getJobs(query?: { limit?: number }, signal?: AbortSignal) {
  return hoservaClient.GET("/jobs", { params: { query }, signal });
}

export function getDoctor(signal?: AbortSignal) {
  return hoservaClient.GET("/doctor", { signal });
}

export function getDisks(signal?: AbortSignal) {
  return hoservaClient.GET("/disks", { signal });
}

export function getExternalDisks(signal?: AbortSignal) {
  return hoservaClient.GET("/disks/external", { signal });
}

export function getParity(signal?: AbortSignal) {
  return hoservaClient.GET("/parity", { signal });
}

export function getJob(jobId: string, signal?: AbortSignal) {
  return hoservaClient.GET("/jobs/{jobId}", { params: { path: { jobId } }, signal });
}

export function getJobLog(jobId: string, signal?: AbortSignal) {
  return hoservaClient.GET("/jobs/{jobId}/log", { params: { path: { jobId } }, signal });
}

export function getNotifications(signal?: AbortSignal) {
  return hoservaClient.GET("/notifications", { signal });
}

export function getMetrics(
  query: { metric: string; from: string; to: string },
  signal?: AbortSignal,
) {
  return hoservaClient.GET("/metrics", { params: { query }, signal });
}

export function postParityDiff() {
  return hoservaClient.POST("/parity/diff");
}

export function postParitySync() {
  return hoservaClient.POST("/parity/sync", { body: { confirm: true, dryRun: false } });
}

export function postParityFix() {
  return hoservaClient.POST("/parity/fix", { body: { confirm: true } });
}

export function postParityScrub() {
  return hoservaClient.POST("/parity/scrub", { body: { percent: 100 } });
}

export function postJobCancel(jobId: string) {
  return hoservaClient.POST("/jobs/{jobId}/cancel", { params: { path: { jobId } } });
}

export function postNotificationsReadAll() {
  return hoservaClient.POST("/notifications/read", { body: { all: true } });
}

export function postAuthLogout() {
  return hoservaClient.POST("/auth/logout");
}

export function postDisksArray(body: CreateArrayRequest) {
  return hoservaClient.POST("/disks/array", { body });
}

export function patchExternalDisk(label: string, body: { backupDestination: boolean }) {
  return hoservaClient.PATCH("/disks/external/{label}", {
    params: { path: { label } },
    body,
  });
}

export function postExternalDiskMount(label: string) {
  return hoservaClient.POST("/disks/external/{label}/mount", { params: { path: { label } } });
}

export function postExternalDiskEject(label: string) {
  return hoservaClient.POST("/disks/external/{label}/eject", { params: { path: { label } } });
}

export function postExternalDiskFormat(label: string, confirmation: string) {
  return hoservaClient.POST("/disks/external/{label}/format", {
    params: { path: { label } },
    body: { confirmation },
  });
}

export function postArrayAddPlan(body: {
  device: string;
  filesystem: ArrayDiskFilesystem;
  adopt: boolean;
}) {
  return hoservaClient.POST("/disks/array/add/plan", { body });
}

export function postArrayAdd(body: {
  device: string;
  filesystem: ArrayDiskFilesystem;
  adopt: boolean;
  confirmation: string;
}) {
  return hoservaClient.POST("/disks/array/add", { body });
}

export function postArrayReplacePlan(body: {
  mountpoint: string;
  device: string;
  filesystem: ArrayDiskFilesystem;
  adopt: boolean;
}) {
  return hoservaClient.POST("/disks/array/replace/plan", { body });
}

export function postArrayReplace(body: {
  mountpoint: string;
  device: string;
  filesystem: ArrayDiskFilesystem;
  adopt: boolean;
  confirmation: string;
}) {
  return hoservaClient.POST("/disks/array/replace", { body });
}

export function postArrayUpgradePlan(body: {
  mountpoint: string;
  device: string;
  filesystem: ArrayDiskFilesystem;
}) {
  return hoservaClient.POST("/disks/array/upgrade/plan", { body });
}

export function postArrayUpgrade(body: {
  mountpoint: string;
  device: string;
  filesystem: ArrayDiskFilesystem;
  newMountpoint?: string;
  confirmation: string;
}) {
  return hoservaClient.POST("/disks/array/upgrade", { body });
}

export function postArrayStop() {
  return hoservaClient.POST("/array/stop", { body: { confirm: true } });
}

export function postArrayStart() {
  return hoservaClient.POST("/array/start");
}
