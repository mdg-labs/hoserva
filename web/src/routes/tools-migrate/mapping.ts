import type { components } from "@/lib/api/client";
import type { ImportDisk, ReviewDisk } from "@/routes/tools-migrate/report";

export type MappingRole = components["schemas"]["MigrationProposedRole"];

// A row's chosen role, by row key; null is "nothing chosen", which is what a
// row the API proposes no role for starts as.
export type DiskMapping = Record<string, MappingRole | null>;

export const MAPPING_ROLES: MappingRole[] = ["parity", "data", "cache", "ignore"];

const MIN_PARITY_DISKS = 1;
const MAX_PARITY_DISKS = 2;

export type Violation = "refused_data" | "weak_parity" | "boot_device" | "parity_data" | "host_boot_parity";

export function diskKey(disk: ReviewDisk, index: number): string {
  return `${index}|${disk.slot ?? ""}|${disk.serial ?? disk.device ?? ""}`;
}

export function proposedMapping(disks: ReviewDisk[]): DiskMapping {
  const mapping: DiskMapping = {};
  disks.forEach((disk, index) => {
    mapping[diskKey(disk, index)] = disk.proposedRole ?? null;
  });
  return mapping;
}

function isUnraidBootDevice(disk: ReviewDisk): boolean {
  return disk.unraidBoot === true || disk.unraidRole === "boot";
}

// These mirror the rules the import enforces on the mapping, to say so while
// the roles are still being chosen; they add none of their own.
export function diskViolations(disk: ReviewDisk, role: MappingRole | null): Violation[] {
  const found: Violation[] = [];
  if (role === "data" && disk.refused) {
    found.push("refused_data");
  }
  if (role === "parity" && disk.weakIdentity === true) {
    found.push("weak_parity");
  }
  if (role === "parity" && disk.refusalCode === "host_boot") {
    found.push("host_boot_parity");
  }
  if (role !== null && role !== "ignore" && isUnraidBootDevice(disk)) {
    const sharedCache = disk.unraidBoot === true && disk.unraidRole === "cache" && role === "cache";
    if (!sharedCache) {
      found.push("boot_device");
    }
  }
  if (role === "data" && disk.unraidRole === "parity") {
    found.push("parity_data");
  }
  return found;
}

export type ParityProblem = "none" | "tooMany";

export function parityCount(disks: ReviewDisk[], mapping: DiskMapping): number {
  return disks.filter((disk, index) => mapping[diskKey(disk, index)] === "parity").length;
}

export function parityProblem(count: number): ParityProblem | null {
  if (count < MIN_PARITY_DISKS) {
    return "none";
  }
  return count > MAX_PARITY_DISKS ? "tooMany" : null;
}

export interface CachePartition {
  device: string;
  byIdName: string;
  partUuid: string;
}

export type ImportBlocker = "violations" | "parity" | "noRoles" | "noIdentity" | "needsPartition";

// The mapping the import takes, built as the CLI builds it: a disk is named by
// WWN, else serial, and a disk left out or ignored is not sent. The cache on the
// disk this machine boots from is a spare partition of it, named by its by-id
// link and PARTUUID, never the whole disk.
export function importRoles(
  disks: ReviewDisk[],
  mapping: DiskMapping,
  partition: CachePartition | null,
): { roles: ImportDisk[]; blocker: ImportBlocker | null } {
  const roles: ImportDisk[] = [];
  let blocker: ImportBlocker | null = null;
  const block = (next: ImportBlocker): void => {
    blocker ??= next;
  };
  disks.forEach((disk, index) => {
    const role = mapping[diskKey(disk, index)] ?? null;
    if (role === null || role === "ignore") {
      return;
    }
    if (diskViolations(disk, role).length > 0) {
      block("violations");
      return;
    }
    if (role === "cache" && disk.hostBoot === true) {
      if (partition === null) {
        block("needsPartition");
        return;
      }
      roles.push({ role, byId: partition.byIdName, partUuid: partition.partUuid });
      return;
    }
    if (disk.wwn) {
      roles.push({ role, wwn: disk.wwn });
    } else if (disk.serial) {
      roles.push({ role, serial: disk.serial });
    } else {
      block("noIdentity");
    }
  });
  if (parityProblem(parityCount(disks, mapping)) !== null) {
    block("parity");
  }
  if (roles.length === 0) {
    block("noRoles");
  }
  return { roles, blocker };
}

export function needsCachePartition(disks: ReviewDisk[], mapping: DiskMapping): ReviewDisk | undefined {
  return disks.find((disk, index) => mapping[diskKey(disk, index)] === "cache" && disk.hostBoot === true);
}
