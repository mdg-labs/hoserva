import { hoservaClient, type components } from "@/lib/api/client";

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
