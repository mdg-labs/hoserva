import type { CreatePolicy, DiskEntry, DiskRole, FilesystemChoice } from "./validation";
import { assignableDisks } from "./validation";

export interface ArraySetupState {
  disks: DiskEntry[];
  roles: Record<string, DiskRole>;
  filesystemChoices: Record<string, FilesystemChoice>;
  createPolicy: CreatePolicy;
  minFreeSpaceGb: number;
}

export interface ConfigPreview {
  snapraidConf: string;
  mergerfsUnit: string;
  summary: ArraySummary;
}

export interface ArraySummary {
  totalDisks: number;
  dataDiskCount: number;
  parityDiskCount: number;
  cacheDiskCount: number;
  usableBytes: number;
  faultToleranceLabel: "none" | "single" | "dual";
  paritySizeBytes: number;
}

function dataMountPoint(index: number): string {
  return `/mnt/disk${index}`;
}

function parityMountPoint(index: number): string {
  return index === 0 ? "/mnt/parity" : `/mnt/parity${index + 1}`;
}

export function summarizeArray(state: ArraySetupState): ArraySummary {
  const candidates = assignableDisks(state.disks);
  const dataDisks = candidates.filter((disk) => state.roles[disk.device] === "data");
  const parityDisks = candidates.filter((disk) => state.roles[disk.device] === "parity");
  const cacheDisks = candidates.filter((disk) => state.roles[disk.device] === "cache");

  const usableBytes = dataDisks.reduce((total, disk) => total + disk.sizeBytes, 0);
  const paritySizeBytes =
    parityDisks.length > 0 ? Math.max(...parityDisks.map((disk) => disk.sizeBytes)) : 0;

  let faultToleranceLabel: ArraySummary["faultToleranceLabel"] = "none";
  if (parityDisks.length === 1) {
    faultToleranceLabel = "single";
  } else if (parityDisks.length === 2) {
    faultToleranceLabel = "dual";
  }

  return {
    totalDisks: dataDisks.length + parityDisks.length + cacheDisks.length,
    dataDiskCount: dataDisks.length,
    parityDiskCount: parityDisks.length,
    cacheDiskCount: cacheDisks.length,
    usableBytes,
    faultToleranceLabel,
    paritySizeBytes,
  };
}

function contentPaths(state: ArraySetupState): string[] {
  const paths = ["/var/lib/hoserva/snapraid.content"];
  const cacheDisks = assignableDisks(state.disks).filter((disk) => state.roles[disk.device] === "cache");
  if (cacheDisks.length > 0) {
    paths.push("/mnt/cache/snapraid.content");
  }

  const dataDisks = assignableDisks(state.disks)
    .filter((disk) => state.roles[disk.device] === "data")
    .sort((left, right) => right.sizeBytes - left.sizeBytes);

  const parityCount = assignableDisks(state.disks).filter((disk) => state.roles[disk.device] === "parity").length;
  const targetCount = Math.max(0, parityCount + 2 - paths.length);
  for (let index = 0; index < Math.min(targetCount, dataDisks.length); index += 1) {
    paths.push(`${dataMountPoint(index + 1)}/snapraid.content`);
  }

  return paths;
}

export function buildConfigPreview(state: ArraySetupState): ConfigPreview {
  const summary = summarizeArray(state);
  const dataDisks = assignableDisks(state.disks).filter((disk) => state.roles[disk.device] === "data");
  const parityDisks = assignableDisks(state.disks).filter((disk) => state.roles[disk.device] === "parity");

  const snapraidLines: string[] = [];
  parityDisks.forEach((_disk, index) => {
    const mount = parityMountPoint(index);
    if (index === 0) {
      snapraidLines.push(`parity ${mount}/snapraid.parity`);
      return;
    }
    snapraidLines.push(`2-parity ${mount}/snapraid.2-parity`);
  });

  for (const contentPath of contentPaths(state)) {
    snapraidLines.push(`content ${contentPath}`);
  }

  dataDisks.forEach((_disk, index) => {
    snapraidLines.push(`data d${index + 1} ${dataMountPoint(index + 1)}`);
  });

  snapraidLines.push(
    "exclude /lost+found/",
    "exclude /.Trash-*/",
    "exclude /appdata/",
    "exclude *.unrecoverable",
    "exclude /snapraid.content*",
    "exclude *.hoserva-moving-*",
    "exclude .DS_Store",
  );

  const mergerfsBranches = dataDisks.map((_, index) => dataMountPoint(index + 1)).join(":");
  const mergerfsUnit = `[Unit]
Description=Hoserva data pool
After=local-fs.target
RequiresMountsFor=${mergerfsBranches.replaceAll(":", " ")}

[Mount]
What=${mergerfsBranches}
Where=/mnt/user
Type=fuse.mergerfs
Options=category.create=${state.createPolicy},minfreespace=${state.minFreeSpaceGb}G,moveonenospc=true,dropcacheonclose=true,cache.files=partial,fsname=hoserva-pool

[Install]
WantedBy=multi-user.target`;

  return {
    snapraidConf: `${snapraidLines.join("\n")}\n`,
    mergerfsUnit,
    summary,
  };
}

export function formatBytes(bytes: number): string {
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  if (bytes === 0) {
    return "0 B";
  }
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / 1024 ** exponent;
  const digits = value >= 100 || exponent === 0 ? 0 : value >= 10 ? 1 : 2;
  return `${value.toFixed(digits)} ${units[exponent]}`;
}
