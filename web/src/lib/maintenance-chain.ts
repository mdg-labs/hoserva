export type MaintenanceChainStepId =
  | "mover"
  | "diff_guard"
  | "sync"
  | "scrub"
  | "config_backup";

export const MAINTENANCE_CHAIN_STEPS: MaintenanceChainStepId[] = [
  "mover",
  "diff_guard",
  "sync",
  "scrub",
  "config_backup",
];

export type OtherScheduleJobId =
  | "smart_self_test"
  | "appdata_backup"
  | "restore_drill"
  | "container_update_check";

export const OTHER_SCHEDULE_JOBS: OtherScheduleJobId[] = [
  "smart_self_test",
  "appdata_backup",
  "restore_drill",
  "container_update_check",
];
