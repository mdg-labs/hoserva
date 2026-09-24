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

// ipv6TokensToGroups expands a run of ':'-split IPv6 tokens into 16-bit
// groups, resolving a trailing embedded-IPv4 token ("1.2.3.4") into its
// two groups. Returns null for anything that isn't a valid token run.
function ipv6TokensToGroups(tokens: string[]): number[] | null {
  const groups: number[] = [];
  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i];
    if (t.includes(".")) {
      if (i !== tokens.length - 1) {
        return null;
      }
      const octets = t.split(".");
      if (octets.length !== 4 || octets.some((o) => !/^\d{1,3}$/.test(o) || Number(o) > 255)) {
        return null;
      }
      const nums = octets.map((o) => Number(o));
      groups.push((nums[0] << 8) | nums[1], (nums[2] << 8) | nums[3]);
      continue;
    }
    if (!/^[0-9a-fA-F]{1,4}$/.test(t)) {
      return null;
    }
    groups.push(Number.parseInt(t, 16));
  }
  return groups;
}

// parseIPv6Groups parses addr (no zone id, no brackets) into its 8 16-bit
// groups, expanding a single "::" run, mirroring what net.ParseIP accepts
// for an IPv6 address. Returns null for anything it doesn't accept.
function parseIPv6Groups(addr: string): number[] | null {
  const firstDouble = addr.indexOf("::");
  if (firstDouble !== addr.lastIndexOf("::")) {
    return null;
  }
  if (firstDouble === -1) {
    const groups = ipv6TokensToGroups(addr.split(":"));
    return groups && groups.length === 8 ? groups : null;
  }
  const head = addr.slice(0, firstDouble);
  const tail = addr.slice(firstDouble + 2);
  const headGroups = ipv6TokensToGroups(head === "" ? [] : head.split(":"));
  const tailGroups = ipv6TokensToGroups(tail === "" ? [] : tail.split(":"));
  if (headGroups === null || tailGroups === null) {
    return null;
  }
  const total = headGroups.length + tailGroups.length;
  if (total > 7) {
    return null;
  }
  return [...headGroups, ...new Array<number>(8 - total).fill(0), ...tailGroups];
}

// isIPv4Mapped mirrors net.IP.To4()'s ::ffff:a.b.c.d check: an address
// Go treats as IPv4, never as a bracketed IPv6 token.
function isIPv4Mapped(groups: number[]): boolean {
  return groups[0] === 0 && groups[1] === 0 && groups[2] === 0 && groups[3] === 0 && groups[4] === 0 && groups[5] === 0xffff;
}

// canonicalIPv6 mirrors net.IP.String() for a genuine (non-IPv4-mapped)
// IPv6 address: lowercase hex groups with no leading zeros, and the
// longest run of two or more all-zero groups — leftmost on a tie —
// collapsed to "::". Returns null for anything nfsClientToken should
// leave untouched (invalid, or IPv4-mapped).
function canonicalIPv6(addr: string): string | null {
  const groups = parseIPv6Groups(addr);
  if (groups === null || isIPv4Mapped(groups)) {
    return null;
  }
  let bestStart = -1;
  let bestLen = 0;
  for (let i = 0; i < 8; ) {
    if (groups[i] !== 0) {
      i++;
      continue;
    }
    let j = i;
    while (j < 8 && groups[j] === 0) {
      j++;
    }
    if (j - i > bestLen) {
      bestStart = i;
      bestLen = j - i;
    }
    i = j;
  }
  if (bestLen < 2) {
    return groups.map((g) => g.toString(16)).join(":");
  }
  const before = groups.slice(0, bestStart).map((g) => g.toString(16));
  const after = groups.slice(bestStart + bestLen).map((g) => g.toString(16));
  return `${before.join(":")}::${after.join(":")}`;
}

// nfsClientToken mirrors internal/config.nfsClientToken: IPv6 addresses
// and networks are square-bracketed in an exports(5) client field;
// hostnames, IPv4 addresses and IPv4 CIDRs are emitted as given. A
// colon appears in an IPv6 token and nowhere else a valid host field
// can appear (a DNS hostname, IPv4 address or IPv4 CIDR never contains
// one), so it is enough to tell the two apart. A plain IPv6 address is
// bracketed verbatim, exactly as nfsClientToken leaves it — only a
// CIDR's address is canonicalized, because that's the only case Go's
// nfsClientToken runs through net.ParseCIDR + ip.String() before
// bracketing; a stored host typed in non-canonical form (pasted
// uncompressed, no "::" shorthand) would otherwise still render
// differently here than in /etc/exports. Both branches leave an
// IPv4-mapped address (which Go's To4() treats as IPv4) and anything
// net.ParseIP rejects unbracketed, as Go does.
function nfsClientToken(host: string): string {
  if (!host.includes(":")) {
    return host;
  }
  const slash = host.lastIndexOf("/");
  if (slash === -1) {
    const groups = parseIPv6Groups(host);
    if (groups === null || isIPv4Mapped(groups)) {
      return host;
    }
    return `[${host}]`;
  }
  const addr = host.slice(0, slash);
  const canonical = canonicalIPv6(addr);
  if (canonical === null) {
    return host;
  }
  return `[${canonical}]${host.slice(slash)}`;
}

// Mirrors internal/config.RenderNFSExports's per-share line (doc 03 §4.2
// "export line preview"). nfs.fsid is backend-owned (#350) — the caller
// gets it from the saved share's GET response, never derives it itself
// (#351). Without a saved fsid there is nothing to preview yet.
export function buildNfsExportLine(path: string, nfs: ShareNFS): string {
  // internal/config.RenderNFSExports sorts hosts with Go's sort.Strings —
  // ascending byte order. localeCompare is a collation, not a byte
  // order, and puts an IPv6 host's ":" ahead of an IPv6 CIDR's own
  // digits, so it can print the two clients on a line in the opposite
  // order hoservad wrote them.
  const hosts = [...nfs.hosts].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
  if (hosts.length === 0 || !nfs.fsid) {
    return path;
  }
  return `${path} ${hosts
    .map((host) => `${nfsClientToken(host)}(rw,sync,no_subtree_check,fsid=${nfs.fsid},${nfs.squash})`)
    .join(" ")}`;
}
