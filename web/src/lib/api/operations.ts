import { hoservaClient, type components } from "@/lib/api/client";
import type { ClientResult } from "@/lib/api/request";

type BackupDestination = components["schemas"]["BackupDestination"];
type CreateBackupDestinationRequest = components["schemas"]["CreateBackupDestinationRequest"];
type UpdateBackupDestinationRequest = components["schemas"]["UpdateBackupDestinationRequest"];
type RestoreDrill = components["schemas"]["RestoreDrill"];

type ArrayDiskFilesystem = components["schemas"]["ArrayDiskFilesystem"];
type CreateArrayRequest = components["schemas"]["CreateArrayRequest"];
type UpdateShareRequest = components["schemas"]["UpdateShareRequest"];
type UpdateSharePermissionsRequest = components["schemas"]["UpdateSharePermissionsRequest"];
type ApiTokenRole = components["schemas"]["ApiTokenRole"];
type UpdateUserSharePermissionsRequest = components["schemas"]["UpdateUserSharePermissionsRequest"];
type EditableUserRole = "viewer" | "share-only";
type NotificationChannelType = components["schemas"]["NotificationChannelType"];
type ApplyNetworkSettingsRequest = components["schemas"]["ApplyNetworkSettingsRequest"];
type DNS01Provider = components["schemas"]["DNS01Provider"];
type UpdateChannel = NonNullable<components["schemas"]["UpdateStatus"]["channel"]>;
type MaintenanceChainStep = components["schemas"]["MaintenanceChainStep"];
type ScheduleFrequency = components["schemas"]["ScheduleFrequency"];
type UpdateGeneralSettingsRequest = components["schemas"]["UpdateGeneralSettingsRequest"];
type UpdateUPSSettingsRequest = components["schemas"]["UpdateUPSSettingsRequest"];
type ApplyHostConfigRequest = components["schemas"]["ApplyHostConfigRequest"];

export function getStatus(signal?: AbortSignal) {
  return hoservaClient.GET("/status", { signal });
}

export function getPool(signal?: AbortSignal) {
  return hoservaClient.GET("/pool", { signal });
}

type JobClass = components["schemas"]["JobClass"];
type JobStatus = components["schemas"]["JobStatus"];

export function getJobs(
  query?: { class?: JobClass; status?: JobStatus; limit?: number },
  signal?: AbortSignal,
) {
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

const GZIP_MAGIC = [0x1f, 0x8b];
const JOB_LOG_NOT_FOUND_CODE = "job_log_not_found";

// A log still being written has no gzip trailer, so the decompressor ends in
// an error after it has already produced the lines written so far. Those
// lines are the log; an error before any output is a corrupt stream.
async function gunzipText(bytes: Uint8Array<ArrayBuffer>): Promise<string> {
  const stream = new DecompressionStream("gzip");
  const writer = stream.writable.getWriter();
  void writer.write(bytes).catch(() => undefined);
  void writer.close().catch(() => undefined);

  const reader = stream.readable.getReader();
  const decoder = new TextDecoder();
  let text = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        return text + decoder.decode();
      }
      text += decoder.decode(value, { stream: true });
    }
  } catch (error: unknown) {
    if (text === "") {
      throw error;
    }
    return text + decoder.decode();
  }
}

// The response is gzip (`application/gzip`) that the daemon sends without a
// Content-Encoding, so the browser leaves it compressed and it is inflated
// here. A body that is already plain text (a proxy that decoded it) is
// returned as it is.
export async function getJobLog(jobId: string, signal?: AbortSignal): Promise<ClientResult<string>> {
  const result = await hoservaClient.GET("/jobs/{jobId}/log", {
    params: { path: { jobId } },
    parseAs: "arrayBuffer",
    signal,
  });
  if (result.error?.code === JOB_LOG_NOT_FOUND_CODE) {
    return { data: undefined, response: { ok: true } };
  }
  if (result.error !== undefined || result.response?.ok === false) {
    return { error: result.error, response: { ok: false } };
  }
  if (result.data === undefined) {
    return { data: undefined, response: { ok: true } };
  }
  const bytes = new Uint8Array(result.data);
  const isGzip = bytes[0] === GZIP_MAGIC[0] && bytes[1] === GZIP_MAGIC[1];
  const text = isGzip ? await gunzipText(bytes) : new TextDecoder().decode(bytes);
  return { data: text, response: { ok: true } };
}

export function getApps(signal?: AbortSignal) {
  return hoservaClient.GET("/apps", { signal });
}

export function getAppUpdates(signal?: AbortSignal) {
  return hoservaClient.GET("/apps/updates", { signal });
}

export function getApp(id: string, signal?: AbortSignal) {
  return hoservaClient.GET("/apps/{id}", { params: { path: { id } }, signal });
}

export function postAppRecreate(id: string) {
  return hoservaClient.POST("/apps/{id}/recreate", { params: { path: { id } } });
}

// deleteAppdata is sent only when it is true: the daemon reads an absent
// parameter as "keep the appdata", which is the safe choice.
export function deleteApp(id: string, deleteAppdata: boolean) {
  return hoservaClient.DELETE("/apps/{id}", {
    params: { path: { id }, query: deleteAppdata ? { deleteAppdata: true } : {} },
  });
}

export function deleteStack(name: string, deleteAppdata: boolean) {
  return hoservaClient.DELETE("/stacks/{name}", {
    params: { path: { name }, query: deleteAppdata ? { deleteAppdata: true } : {} },
  });
}

export function postAppStart(id: string) {
  return hoservaClient.POST("/apps/{id}/start", { params: { path: { id } } });
}

export function postAppStop(id: string) {
  return hoservaClient.POST("/apps/{id}/stop", { params: { path: { id } } });
}

export function postAppRestart(id: string) {
  return hoservaClient.POST("/apps/{id}/restart", { params: { path: { id } } });
}

// The log body is plain text, so it is read as text rather than parsed as
// the client's default JSON.
export function getAppLogs(id: string, tail: number, signal?: AbortSignal) {
  return hoservaClient.GET("/apps/{id}/logs", {
    params: { path: { id }, query: { tail } },
    parseAs: "text",
    signal,
  });
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

export function postAcknowledgeDegradedArray() {
  return hoservaClient.POST("/array/degraded/acknowledge");
}

export function postPoolRebalancePlan() {
  return hoservaClient.POST("/pool/rebalance/plan");
}

export function postPoolRebalance(body: { confirmation: string }) {
  return hoservaClient.POST("/pool/rebalance", { body });
}

export function postDiskEvacuationPlan(body: { mountpoint: string }) {
  return hoservaClient.POST("/disks/array/evacuate/plan", { body });
}

export function postDiskEvacuation(body: { mountpoint: string; confirmation: string }) {
  return hoservaClient.POST("/disks/array/evacuate", { body });
}

export function postDiskRemovalFinish(body: { mountpoint: string; confirmation: string }) {
  return hoservaClient.POST("/disks/array/remove/finish", { body });
}

export function postDiskRemovalCancel(body: { mountpoint: string }) {
  return hoservaClient.POST("/disks/array/remove/cancel", { body });
}

export function postMoverRun() {
  return hoservaClient.POST("/mover/run");
}

export function getWakeEvents(signal?: AbortSignal) {
  return hoservaClient.GET("/disks/wake-events", { signal });
}

// Auth and setup — auth-guard.tsx's own startup probe.

export function getSetupStatus(signal?: AbortSignal) {
  return hoservaClient.GET("/setup/status", { signal });
}

export function getAuthSession(signal?: AbortSignal) {
  return hoservaClient.GET("/auth/session", { signal });
}

export function postAuthLogin(body: { username: string; password: string; totpCode?: string }) {
  return hoservaClient.POST("/auth/login", { body });
}

export function postSetupAdmin(body: { username: string; password: string }) {
  return hoservaClient.POST("/setup/admin", { body });
}

export function postAuthTotpEnroll() {
  return hoservaClient.POST("/auth/totp/enroll", { body: {} });
}

export function postAuthTotpConfirm(code: string) {
  return hoservaClient.POST("/auth/totp/confirm", { body: { code } });
}

// Users, groups, sessions and API tokens.

export function getUsers(signal?: AbortSignal) {
  return hoservaClient.GET("/users", { signal });
}

export function getUserGroups(signal?: AbortSignal) {
  return hoservaClient.GET("/user-groups", { signal });
}

export function getSessions(signal?: AbortSignal) {
  return hoservaClient.GET("/sessions", { signal });
}

export function getApiTokens(signal?: AbortSignal) {
  return hoservaClient.GET("/api-tokens", { signal });
}

export function getUserSharePermissions(userId: string, signal?: AbortSignal) {
  return hoservaClient.GET("/users/{userId}/permissions", { params: { path: { userId } }, signal });
}

export function postUser(body: { username: string; role: EditableUserRole }) {
  return hoservaClient.POST("/users", { body });
}

export function patchUser(userId: string, body: { role: EditableUserRole }) {
  return hoservaClient.PATCH("/users/{userId}", { params: { path: { userId } }, body });
}

export function deleteUser(userId: string) {
  return hoservaClient.DELETE("/users/{userId}", { params: { path: { userId } } });
}

export function postUserPassword(userId: string, password: string) {
  return hoservaClient.POST("/users/{userId}/password", { params: { path: { userId } }, body: { password } });
}

export function putUserSharePermissions(userId: string, body: UpdateUserSharePermissionsRequest) {
  return hoservaClient.PUT("/users/{userId}/permissions", { params: { path: { userId } }, body });
}

export function postUserGroup(name: string) {
  return hoservaClient.POST("/user-groups", { body: { name } });
}

export function deleteUserGroup(groupId: string) {
  return hoservaClient.DELETE("/user-groups/{groupId}", { params: { path: { groupId } } });
}

export function putUserGroupMembers(groupId: string, userIds: string[]) {
  return hoservaClient.PUT("/user-groups/{groupId}/members", {
    params: { path: { groupId } },
    body: { userIds },
  });
}

export function postUserToken(username: string, body: { name: string; role: ApiTokenRole }) {
  return hoservaClient.POST("/users/{username}/tokens", { params: { path: { username } }, body });
}

export function deleteSession(sessionId: string) {
  return hoservaClient.DELETE("/sessions/{sessionId}", { params: { path: { sessionId } } });
}

export function deleteApiToken(tokenId: string) {
  return hoservaClient.DELETE("/api-tokens/{tokenId}", { params: { path: { tokenId } } });
}

// Shares.

export function getShares(signal?: AbortSignal) {
  return hoservaClient.GET("/shares", { signal });
}

export function getShare(name: string, signal?: AbortSignal) {
  return hoservaClient.GET("/shares/{name}", { params: { path: { name } }, signal });
}

export function getSharePermissions(name: string, signal?: AbortSignal) {
  return hoservaClient.GET("/shares/{name}/permissions", { params: { path: { name } }, signal });
}

export function postShare(name: string) {
  return hoservaClient.POST("/shares", { body: { name } });
}

export function patchShare(name: string, body: UpdateShareRequest) {
  return hoservaClient.PATCH("/shares/{name}", { params: { path: { name } }, body });
}

export function deleteShare(name: string) {
  return hoservaClient.DELETE("/shares/{name}", { params: { path: { name } }, body: { confirm: true } });
}

export function postShareDataDelete(name: string, confirmation: string) {
  return hoservaClient.POST("/shares/{name}/data/delete", {
    params: { path: { name } },
    body: { confirmation },
  });
}

export function postShareRelocate(name: string, to: "cache" | "array") {
  return hoservaClient.POST("/shares/{name}/relocate", { params: { path: { name } }, body: { to } });
}

export function putSharePermissions(name: string, body: UpdateSharePermissionsRequest) {
  return hoservaClient.PUT("/shares/{name}/permissions", { params: { path: { name } }, body });
}

export function getShareBrowse(name: string, path: string, signal?: AbortSignal) {
  return hoservaClient.GET("/shares/{name}/browse", { params: { path: { name }, query: { path } }, signal });
}

export function deleteShareFile(name: string, path: string) {
  return hoservaClient.DELETE("/shares/{name}/browse", {
    params: { path: { name }, query: { path } },
    body: { confirm: true },
  });
}

// Notifications settings.

export function getNotificationChannels(signal?: AbortSignal) {
  return hoservaClient.GET("/notifications/channels", { signal });
}

export function getNotificationRouting(signal?: AbortSignal) {
  return hoservaClient.GET("/notifications/routing", { signal });
}

export function getQuietHours(signal?: AbortSignal) {
  return hoservaClient.GET("/notifications/quiet-hours", { signal });
}

export function postNotificationChannel(body: {
  name: string;
  type: NotificationChannelType;
  enabled: boolean;
  secret?: string;
}) {
  return hoservaClient.POST("/notifications/channels", { body });
}

export function putNotificationChannel(
  channelId: string,
  body: { name: string; type: NotificationChannelType; enabled: boolean },
) {
  return hoservaClient.PUT("/notifications/channels/{channelId}", { params: { path: { channelId } }, body });
}

export function postNotificationChannelTest(channelId: string) {
  return hoservaClient.POST("/notifications/channels/{channelId}/test", { params: { path: { channelId } } });
}

export function putNotificationRoute(
  eventType: components["schemas"]["NotificationEventType"],
  body: components["schemas"]["UpdateNotificationRouteRequest"],
) {
  return hoservaClient.PUT("/notifications/routing/{eventType}", {
    params: { path: { eventType } },
    body,
  });
}

export function putQuietHours(body: { enabled: boolean; start: string; end: string }) {
  return hoservaClient.PUT("/notifications/quiet-hours", { body });
}

// Network settings.

export function getNetworkSettings(signal?: AbortSignal) {
  return hoservaClient.GET("/settings/network", { signal });
}

export function putNetworkSettings(body: ApplyNetworkSettingsRequest) {
  return hoservaClient.PUT("/settings/network", { body });
}

export function postNetworkSettingsConfirm() {
  return hoservaClient.POST("/settings/network/confirm", {});
}

export function postNetworkCertificateRegen() {
  return hoservaClient.POST("/settings/network/certificate", {});
}

export function postNetworkLetsEncrypt(body: {
  domain: string;
  provider: DNS01Provider;
  cloudflareAPIToken?: string;
  rfc2136Nameserver?: string;
  rfc2136TsigKeyName?: string;
  rfc2136TsigSecret?: string;
}) {
  return hoservaClient.POST("/settings/network/lets-encrypt", { body });
}

export function deleteNetworkLetsEncrypt() {
  return hoservaClient.DELETE("/settings/network/lets-encrypt", {});
}

// Update settings.

export function getUpdateStatus(signal?: AbortSignal) {
  return hoservaClient.GET("/settings/updates", { signal });
}

export function putUpdateSettings(body: { channel?: UpdateChannel; checkEnabled?: boolean }) {
  return hoservaClient.PUT("/settings/updates", { body });
}

export function postUpdateApply() {
  return hoservaClient.POST("/settings/updates/apply", { body: { confirm: true } });
}

export function postUpdateRollback() {
  return hoservaClient.POST("/settings/updates/rollback", { body: { confirm: true } });
}

export function postUpdateReboot() {
  return hoservaClient.POST("/settings/updates/reboot", { body: { confirm: true } });
}

// Schedules settings.

export function getSchedules(signal?: AbortSignal) {
  return hoservaClient.GET("/settings/schedules", { signal });
}

export function putMaintenanceChainSchedule(steps: MaintenanceChainStep[]) {
  return hoservaClient.PUT("/settings/schedules/chain", { body: { steps } });
}

export function putScheduledJob(
  jobId: components["schemas"]["OtherScheduleJobId"],
  patch: { enabled?: boolean; frequency?: ScheduleFrequency; time?: string },
) {
  return hoservaClient.PUT("/settings/schedules/jobs/{jobId}", {
    params: { path: { jobId } },
    body: patch,
  });
}

// General and UPS settings.

export function getGeneralSettings(signal?: AbortSignal) {
  return hoservaClient.GET("/settings/general", { signal });
}

export function getUPSSettings(signal?: AbortSignal) {
  return hoservaClient.GET("/settings/ups", { signal });
}

export function putGeneralSettings(body: UpdateGeneralSettingsRequest) {
  return hoservaClient.PUT("/settings/general", { body });
}

export function putUPSSettings(body: UpdateUPSSettingsRequest) {
  return hoservaClient.PUT("/settings/ups", { body });
}

// Onboarding (welcome) — doctor host-config choices.

export function postDoctorHostConfig(body: ApplyHostConfigRequest) {
  return hoservaClient.POST("/doctor/host-config", { body });
}

// Backup and restore settings.

// A daemon without a backup service answers 501 `not_configured`; that is
// a state the backup UI names, not a load failure, so it resolves to
// `{ available: false }` and every other failure stays an error.
export type Availability<T> = { available: true; value: T } | { available: false };

const NOT_CONFIGURED_CODE = "not_configured";

async function availableUnlessNotConfigured<T>(
  call: Promise<ClientResult<T>>,
): Promise<ClientResult<Availability<T>>> {
  const result = await call;
  if (result.error?.code === NOT_CONFIGURED_CODE) {
    return { data: { available: false }, response: { ok: true } };
  }
  if (result.error !== undefined || result.response?.ok === false || result.data === undefined) {
    return { error: result.error, response: { ok: false } };
  }
  return { data: { available: true, value: result.data }, response: { ok: true } };
}

export function getBackupDestinations(signal?: AbortSignal) {
  return availableUnlessNotConfigured<{ destinations: BackupDestination[] }>(
    hoservaClient.GET("/backup/destinations", { signal }),
  );
}

export function postBackupDestination(body: CreateBackupDestinationRequest) {
  return hoservaClient.POST("/backup/destinations", { body });
}

export function patchBackupDestination(destinationId: string, body: UpdateBackupDestinationRequest) {
  return hoservaClient.PATCH("/backup/destinations/{destinationId}", {
    params: { path: { destinationId } },
    body,
  });
}

export function deleteBackupDestination(destinationId: string) {
  return hoservaClient.DELETE("/backup/destinations/{destinationId}", {
    params: { path: { destinationId } },
  });
}

export function postBackupDestinationTest(destinationId: string) {
  return hoservaClient.POST("/backup/destinations/{destinationId}/test", {
    params: { path: { destinationId } },
  });
}

export function getRestoreDrill(signal?: AbortSignal) {
  return availableUnlessNotConfigured<RestoreDrill>(hoservaClient.GET("/backup/drill", { signal }));
}

export function postRestoreDrill() {
  return hoservaClient.POST("/backup/drill");
}

export function postConfigExport() {
  return hoservaClient.POST("/config/export", { parseAs: "blob" });
}

type AppdataBackupContainer = components["schemas"]["AppdataBackupContainer"];

// `getAppdataBackup` answers 501 `not_configured` without a Docker Engine
// client and 503 when the Engine is not reachable; both are states the
// appdata section names, not load failures.
export type AppdataBackupState =
  | { state: "ready"; containers: AppdataBackupContainer[] }
  | { state: "no_engine" }
  | { state: "engine_unreachable"; message: string };

const ENGINE_UNREACHABLE_STATUS = 503;

export async function getAppdataBackup(signal?: AbortSignal): Promise<ClientResult<AppdataBackupState>> {
  const result = await hoservaClient.GET("/appdata/backup", { signal });
  if (result.error?.code === NOT_CONFIGURED_CODE) {
    return { data: { state: "no_engine" }, response: { ok: true } };
  }
  if (result.response?.status === ENGINE_UNREACHABLE_STATUS) {
    return {
      data: { state: "engine_unreachable", message: result.error?.message ?? "" },
      response: { ok: true },
    };
  }
  if (result.error !== undefined || result.response?.ok === false || result.data === undefined) {
    return { error: result.error, response: { ok: false } };
  }
  return { data: { state: "ready", containers: result.data.containers }, response: { ok: true } };
}

export function putAppdataBackupContainer(name: string, body: { stop: boolean; included: boolean }) {
  return hoservaClient.PUT("/appdata/backup/containers/{name}", { params: { path: { name } }, body });
}

export function postAppdataBackup(containers?: string[]) {
  return hoservaClient.POST("/appdata/backup", {
    body: containers === undefined ? {} : { containers },
  });
}

export function getAppdataArchives(signal?: AbortSignal) {
  return availableUnlessNotConfigured(hoservaClient.GET("/appdata/backup/archives", { signal }));
}

export function postAppdataRestorePreview(body: { container: string; archive: string; destinationId: string }) {
  return hoservaClient.POST("/appdata/backup/restore/preview", { body });
}

export function getAppdataRestorePreview(jobId: string, signal?: AbortSignal) {
  return hoservaClient.GET("/appdata/backup/restore/preview/{jobId}", {
    params: { path: { jobId } },
    signal,
  });
}

export function postAppdataRestore(body: { container: string; archive: string; destinationId: string }) {
  return hoservaClient.POST("/appdata/backup/restore", { body: { ...body, confirm: true } });
}

// The generated body type calls the binary `archive` part a string; the
// request itself is the FormData built here, which the client passes
// through untouched.
function configImportForm(archive: File, fields: Record<string, string>): FormData {
  const form = new FormData();
  form.append("archive", archive);
  for (const [name, value] of Object.entries(fields)) {
    form.append(name, value);
  }
  return form;
}

export function postConfigImportPreview(archive: File, passphrase: string, signal?: AbortSignal) {
  const form = configImportForm(archive, passphrase === "" ? {} : { passphrase });
  return hoservaClient.POST("/config/import/preview", {
    body: { archive: archive.name },
    bodySerializer: () => form,
    signal,
  });
}

export function postConfigImport(args: {
  archive: File;
  passphrase: string;
  diskMapping: components["schemas"]["ConfigImportDiskMapping"] | null;
}) {
  const fields: Record<string, string> = { confirm: "true" };
  if (args.passphrase !== "") {
    fields.passphrase = args.passphrase;
  }
  if (args.diskMapping !== null) {
    fields.diskMapping = JSON.stringify(args.diskMapping);
  }
  const form = configImportForm(args.archive, fields);
  return hoservaClient.POST("/config/import", {
    body: { archive: args.archive.name, confirm: true },
    bodySerializer: () => form,
  });
}
