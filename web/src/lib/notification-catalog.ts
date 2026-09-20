import { Bell, Mail, MessageSquare, Webhook, type LucideIcon } from "lucide-react";

import type { components } from "@/lib/api/client";

export type NotificationChannelType = components["schemas"]["NotificationChannelType"];
export type NotificationEventType = components["schemas"]["NotificationEventType"];
export type NotificationLevel = components["schemas"]["NotificationLevel"];

export const CHANNEL_ICONS: Record<NotificationChannelType, LucideIcon> = {
  discord: MessageSquare,
  email: Mail,
  gotify: Bell,
  ntfy: Bell,
  webhook: Webhook,
};

export const NOTIFICATION_EVENT_TYPES: NotificationEventType[] = [
  "smart_warning",
  "smart_failure",
  "disk_offline",
  "array_degraded",
  "sync_succeeded",
  "sync_failed",
  "sync_blocked_threshold",
  "scrub_errors_found",
  "pool_above_threshold",
  "disk_near_minfreespace",
  "cache_above_threshold",
  "mover_skipping_files",
  "config_drift_detected",
  "container_unhealthy",
  "container_update_available",
  "hoserva_update_available",
  "hoserva_update_failed",
  "reboot_required",
  "ups_on_battery",
  "ups_battery_low",
  "login_failure_burst",
  "credential_reset",
  "certificate_expiring",
  "certificate_renewal_failed",
  "config_backup_failed",
  "appdata_backup_failed",
  "backup_destination_stale",
  "restore_drill_failed",
];

export const NOTIFICATION_LEVELS: NotificationLevel[] = [
  "info",
  "warning",
  "error",
  "critical",
];
