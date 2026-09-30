import type { useTranslation } from "react-i18next";

import type { components } from "@/lib/api/client";

type JobType = components["schemas"]["JobType"];
type JobClass = components["schemas"]["JobClass"];
type Translate = ReturnType<typeof useTranslation>["t"];

// A Record keyed by every JobType/JobClass forces a compile error here if the
// generated schema ever adds a value that has no catalog label yet.
const JOB_TYPE_MEMBERS: Record<JobType, true> = {
  sync: true,
  scrub: true,
  fix: true,
  check: true,
  rebalance: true,
  evacuation: true,
  share_relocation: true,
  mover: true,
  vm_disk_relocation: true,
  disk_format: true,
  disk_add: true,
  disk_remove: true,
  disk_replace: true,
  disk_upgrade_data: true,
  disk_upgrade_parity: true,
  pool_remount: true,
  appdata_backup: true,
  appdata_restore: true,
  appdata_restore_preview: true,
  restore_drill: true,
  config_backup: true,
  container_update: true,
  container_recreate: true,
  acme_issue: true,
  vm_start: true,
  vm_stop: true,
  vm_create: true,
  vm_delete: true,
  vm_snapshot: true,
  vm_clone: true,
  vm_migration_import: true,
};

const JOB_CLASS_MEMBERS: Record<JobClass, true> = {
  parity: true,
  array_write: true,
  topology: true,
  service: true,
  vm: true,
};

export const JOB_TYPE_VALUES = Object.keys(JOB_TYPE_MEMBERS) as JobType[];
export const JOB_CLASS_VALUES = Object.keys(JOB_CLASS_MEMBERS) as JobClass[];

export function jobTypeLabel(type: string, t: Translate): string {
  return Object.hasOwn(JOB_TYPE_MEMBERS, type) ? t(`jobs.types.${type}`) : type;
}

export function jobClassLabel(jobClass: string, t: Translate): string {
  return Object.hasOwn(JOB_CLASS_MEMBERS, jobClass) ? t(`jobs.classes.${jobClass}`) : jobClass;
}
