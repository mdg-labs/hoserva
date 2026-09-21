import type { components } from "@/lib/api/client";

type ShareSMB = components["schemas"]["ShareSMB"];
type ShareNFS = components["schemas"]["ShareNFS"];

function yesNo(value: boolean): string {
  return value ? "yes" : "no";
}

// Mirrors internal/config.RenderSambaConf's per-share stanza (doc 03 §4.2
// "export path preview") — a read-only preview, not sent anywhere.
export function buildSmbStanza(name: string, path: string, smb: ShareSMB): string {
  const lines = [
    `[${name}]`,
    `   path = ${path}`,
    `   browseable = ${yesNo(smb.browseable)}`,
    `   read only = ${yesNo(smb.readOnly)}`,
    `   guest ok = ${yesNo(smb.guest)}`,
    "   create mask = 0664",
    "   force create mode = 0664",
    "   directory mask = 2775",
    "   force directory mode = 2775",
    "   force group = users",
  ];
  if (smb.recycle) {
    lines.push(
      "   vfs objects = catia fruit streams_xattr recycle",
      "   recycle:repository = .recycle",
      "   recycle:keeptree = yes",
      "   recycle:versions = yes",
    );
  }
  if (smb.timeMachine) {
    lines.push("   fruit:time machine = yes");
    if (smb.timeMachineMaxSize) {
      lines.push(`   fruit:time machine max size = ${smb.timeMachineMaxSize}`);
    }
  }
  return lines.join("\n");
}

// Mirrors internal/config.RenderNFSExports's per-share line (doc 03 §4.2
// "export line preview").
export function buildNfsExportLine(path: string, nfs: ShareNFS): string {
  const hosts = [...nfs.hosts].sort((a, b) => a.localeCompare(b));
  if (hosts.length === 0) {
    return path;
  }
  return `${path} ${hosts.map((host) => `${host}(rw,sync,no_subtree_check,${nfs.squash})`).join(" ")}`;
}
