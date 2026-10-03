import type { StatusTone } from "@/components/patterns/status-badge";
import type { components } from "@/lib/api/client";

export type Migration = components["schemas"]["Migration"];
export type MigrationReport = components["schemas"]["MigrationReport"];
export type ReportRow = components["schemas"]["MigrationReportRow"];
export type CheckStatus = components["schemas"]["MigrationCheckStatus"];
export type Verdict = components["schemas"]["MigrationVerdict"];
export type MigrationReview = components["schemas"]["MigrationReview"];
export type ReviewDisk = components["schemas"]["MigrationDisk"];
export type SharePreview = components["schemas"]["MigrationSharePreview"];
export type MigrationBoot = components["schemas"]["MigrationBoot"];
export type MigrationTemplates = components["schemas"]["MigrationTemplates"];
export type TemplateClass = components["schemas"]["MigrationTemplateClass"];
export type TemplateStatus = components["schemas"]["MigrationTemplateStatus"];

export const DOCS_UNPROTECTED_WINDOW_URL = "https://hoserva.dev/migrating-from-unraid/before-you-start";
export const DOCS_CAPTURE_URL = "https://hoserva.dev/migrating-from-unraid/the-migration";

export type SourceKind = "zip" | "stick";
export const SOURCE_ZIP: SourceKind = "zip";
export const SOURCE_STICK: SourceKind = "stick";

export const DISK_CHECKS = ["disk_mapping", "disk_identity", "data_disks", "disk_integrity"];
export const BOOT_CHECKS = ["boot_device"];
export const SHARE_CHECKS = ["shares"];

const CAPTURE_CHECK = "capture";
const CONTAINERS_CHECK = "containers";
const DISK_MAPPING_CHECK = "disk_mapping";

export const TEMPLATE_CLASSES: TemplateClass[] = ["autostart", "running", "stopped", "template_only", "unknown"];

export const CLASS_FILTER_DEFAULT = "default";
export const CLASS_FILTER_ALL = "all";

export function statusTone(status: CheckStatus): StatusTone {
  switch (status) {
    case "pass":
      return "success";
    case "info":
      return "info";
    case "refuse":
      return "error";
    default:
      return "warning";
  }
}

export function verdictTone(verdict: Verdict): StatusTone {
  switch (verdict) {
    case "go":
      return "success";
    case "go_with_warnings":
      return "warning";
    default:
      return "error";
  }
}

export function templateClassTone(templateClass: TemplateClass): StatusTone {
  switch (templateClass) {
    case "autostart":
      return "info";
    case "running":
      return "success";
    case "unknown":
      return "warning";
    default:
      return "outline";
  }
}

export function templateStatusTone(status: TemplateStatus): StatusTone {
  switch (status) {
    case "clean":
    case "previewed":
      return "success";
    case "failed":
      return "error";
    default:
      return "warning";
  }
}

export type ResultGroupId = "refuse" | "attention" | "pass";

export const RESULT_GROUP_ORDER: ResultGroupId[] = ["refuse", "attention", "pass"];

export function resultGroupOf(status: CheckStatus): ResultGroupId {
  switch (status) {
    case "refuse":
      return "refuse";
    case "warn":
    case "flag":
      return "attention";
    default:
      return "pass";
  }
}

export function groupRows(rows: ReportRow[]): Record<ResultGroupId, ReportRow[]> {
  const groups: Record<ResultGroupId, ReportRow[]> = { refuse: [], attention: [], pass: [] };
  for (const row of rows) {
    groups[resultGroupOf(row.status)].push(row);
  }
  return groups;
}

export function rowsOfChecks(rows: ReportRow[], checks: string[]): ReportRow[] {
  return rows.filter((row) => checks.includes(row.check));
}

export function hasMatchedDisks(rows: ReportRow[]): boolean {
  return rows.some((row) => row.check === DISK_MAPPING_CHECK && row.status === "pass");
}

export function captureWarning(rows: ReportRow[]): ReportRow | undefined {
  return rows.find((row) => row.check === CAPTURE_CHECK && row.status === "warn");
}

export type CaptureNotice =
  | { state: "missing" | "unreadable" | "stale"; capturedAt?: string }
  | { state: "row"; detail: string };

// A report made before the structured review has only the capture row, whose
// warning is all there is to go on.
export function captureNotice(report: MigrationReport): CaptureNotice | null {
  const capture = report.review?.capture;
  if (capture) {
    return capture.state === "present" ? null : { state: capture.state, capturedAt: capture.capturedAt };
  }
  const row = captureWarning(report.rows);
  return row ? { state: "row", detail: row.detail } : null;
}

export type BootKind = "usb" | "internal_dedicated" | "internal_mirrored" | "internal_shared" | "internal_both" | "internal_unstated" | "unknown";

// The kinds are the rows of doc 05 section 5's rollback table. A mirrored boot
// pool that also holds the cache is a combination the table has no row for
// ("internal_both"), and an internal boot whose capture leaves either fact out
// is "internal_unstated"; neither is promised a rollback.
export function bootKind(boot: MigrationBoot): BootKind {
  if (boot.mode === "usb") {
    return "usb";
  }
  if (boot.mode !== "internal") {
    return "unknown";
  }
  if (boot.mirrored === undefined || boot.sharedWithCache === undefined) {
    return "internal_unstated";
  }
  if (boot.mirrored && boot.sharedWithCache) {
    return "internal_both";
  }
  if (boot.mirrored) {
    return "internal_mirrored";
  }
  return boot.sharedWithCache ? "internal_shared" : "internal_dedicated";
}

export const ROLLBACK_ROWS: Record<BootKind, string[]> = {
  usb: ["separate", "sharedNvme"],
  internal_dedicated: ["another", "thatDevice"],
  internal_mirrored: ["another", "onePairDevice", "bothPairDevices"],
  internal_shared: ["another", "sameNvme"],
  internal_both: [],
  internal_unstated: [],
  unknown: [],
};

export function allTemplatesUnknown(templates: MigrationTemplates): boolean {
  return (
    templates.counts.allTemplates ||
    (templates.templates.length > 0 && templates.templates.every((template) => template.class === "unknown"))
  );
}

export function flaggedContainers(rows: ReportRow[]): ReportRow[] {
  return rows.filter((row) => row.check === CONTAINERS_CHECK && row.status === "flag");
}

export function classFilterMatches(filter: string, templateClass: TemplateClass): boolean {
  if (filter === CLASS_FILTER_ALL) {
    return true;
  }
  if (filter === CLASS_FILTER_DEFAULT) {
    return templateClass !== "template_only";
  }
  return filter === templateClass;
}

export function reportRowKey(row: ReportRow): string {
  return `${row.check}|${row.subject ?? ""}|${row.detail}`;
}
