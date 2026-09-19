import type { components } from "@/lib/api/client";

export type DiskEntry = components["schemas"]["DiskInventoryEntry"] & {
  filesystem?: string;
  label?: string;
  smartStatus?: string;
  containsData?: boolean;
  looksLikeUnraid?: boolean;
};

export type DiskRole = "parity" | "data" | "cache" | "ignore" | "unassigned";

export type CreatePolicy = "mspmfs" | "mfs" | "lfs" | "ff";

export type FilesystemChoice = "format" | "keep";

export type RoleValidationCode =
  | "tooManyParity"
  | "noDataDisk"
  | "noParityDisk"
  | "parityTooSmall"
  | "weakIdentityParity";

export type RoleWarningCode = "weakIdentityData" | "containsData" | "smartIssue" | "looksLikeUnraid";

export interface RoleValidation {
  errorCodes: RoleValidationCode[];
  warningCodes: RoleWarningCode[];
  errorsByDevice: Partial<Record<string, RoleValidationCode>>;
  warningsByDevice: Partial<Record<string, RoleWarningCode>>;
}

const ADOPTABLE_FILESYSTEMS = ["xfs", "ext4", "btrfs"];

export function assignableDisks(disks: DiskEntry[]): DiskEntry[] {
  return disks.filter((disk) => !disk.boot);
}

export function canKeepFilesystem(disk: DiskEntry): boolean {
  if (!disk.filesystem) {
    return false;
  }
  const normalized = disk.filesystem.toLowerCase();
  return ADOPTABLE_FILESYSTEMS.some((filesystem) => normalized.includes(filesystem));
}

export function validateRoleAssignment(
  disks: DiskEntry[],
  roles: Record<string, DiskRole>,
): RoleValidation {
  const errorCodes: RoleValidationCode[] = [];
  const warningCodes: RoleWarningCode[] = [];
  const errorsByDevice: Partial<Record<string, RoleValidationCode>> = {};
  const warningsByDevice: Partial<Record<string, RoleWarningCode>> = {};

  const candidates = assignableDisks(disks);
  const parityDisks = candidates.filter((disk) => roles[disk.device] === "parity");
  const dataDisks = candidates.filter((disk) => roles[disk.device] === "data");

  if (parityDisks.length > 2) {
    errorCodes.push("tooManyParity");
  }
  if (dataDisks.length === 0) {
    errorCodes.push("noDataDisk");
  }
  if (parityDisks.length === 0 && dataDisks.length > 0) {
    errorCodes.push("noParityDisk");
  }

  if (dataDisks.length > 0 && parityDisks.length > 0) {
    const largestData = Math.max(...dataDisks.map((disk) => disk.sizeBytes));
    for (const parityDisk of parityDisks) {
      if (parityDisk.sizeBytes < largestData) {
        errorsByDevice[parityDisk.device] = "parityTooSmall";
        if (!errorCodes.includes("parityTooSmall")) {
          errorCodes.push("parityTooSmall");
        }
      }
      if (parityDisk.weakIdentity) {
        errorsByDevice[parityDisk.device] = "weakIdentityParity";
        if (!errorCodes.includes("weakIdentityParity")) {
          errorCodes.push("weakIdentityParity");
        }
      }
    }
  }

  for (const disk of candidates) {
    const role = roles[disk.device];
    if (disk.weakIdentity && role === "data") {
      warningsByDevice[disk.device] = "weakIdentityData";
      if (!warningCodes.includes("weakIdentityData")) {
        warningCodes.push("weakIdentityData");
      }
    }
    if (disk.containsData) {
      warningsByDevice[disk.device] = "containsData";
      if (!warningCodes.includes("containsData")) {
        warningCodes.push("containsData");
      }
    }
    if (disk.failed || disk.smartStatus?.toLowerCase().includes("fail")) {
      warningsByDevice[disk.device] = "smartIssue";
      if (!warningCodes.includes("smartIssue")) {
        warningCodes.push("smartIssue");
      }
    }
    if (disk.looksLikeUnraid) {
      warningsByDevice[disk.device] = "looksLikeUnraid";
      if (!warningCodes.includes("looksLikeUnraid")) {
        warningCodes.push("looksLikeUnraid");
      }
    }
  }

  return { errorCodes, warningCodes, errorsByDevice, warningsByDevice };
}

export function roleAssignmentValid(validation: RoleValidation): boolean {
  return validation.errorCodes.length === 0 && Object.keys(validation.errorsByDevice).length === 0;
}

export function disksToErase(
  disks: DiskEntry[],
  roles: Record<string, DiskRole>,
  filesystemChoices: Record<string, FilesystemChoice>,
): string[] {
  const erased: string[] = [];
  for (const disk of assignableDisks(disks)) {
    const role = roles[disk.device];
    if (role === "parity") {
      erased.push(disk.device);
      continue;
    }
    if (role === "data" && filesystemChoices[disk.device] !== "keep") {
      erased.push(disk.device);
    }
  }
  return erased.sort();
}

export function buildConfirmPhrase(devices: string[]): string {
  if (devices.length === 0) {
    return "";
  }
  return `erase ${devices.join(", ")}`;
}

export function typedConfirmMatches(value: string, phrase: string): boolean {
  return value === phrase;
}
