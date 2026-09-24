// Proves buildNfsExportLine renders the same line RenderNFSExports
// writes to /etc/exports (doc 03 §4.2, #350, #351) — without this, the
// share page's own preview could drift from what hoservad actually
// exports, exactly as it did before #351. Fixtures are read straight
// from the testdata/configs/nfs-* goldens instead of copied by hand, so
// this test tracks #350's own derivation rather than a second, possibly
// stale, copy of it.
import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";

import type { components } from "@/lib/api/client";
import { buildNfsExportLine } from "@/routes/shares/config-preview";

type ShareNFS = components["schemas"]["ShareNFS"];

interface NFSState {
  shares: { name: string; hosts: string[]; squash: ShareNFS["squash"] }[];
}

const testdataRoot = path.resolve(import.meta.dirname, "../../../../testdata/configs");

function loadShare(fixture: string, shareName: string) {
  const state = JSON.parse(readFileSync(path.join(testdataRoot, fixture, "state.json"), "utf-8")) as NFSState;
  const golden = readFileSync(path.join(testdataRoot, fixture, "exports.golden"), "utf-8");
  const share = state.shares.find((s) => s.name === shareName);
  if (!share) {
    throw new Error(`${fixture}: no share named ${shareName} in state.json`);
  }
  const line = golden
    .split("\n")
    .find((l) => l.startsWith(`/mnt/user/${shareName} `));
  if (!line) {
    throw new Error(`${fixture}: no rendered line for ${shareName} in exports.golden`);
  }
  const fsidMatch = /fsid=([0-9a-f-]+)/.exec(line);
  if (!fsidMatch) {
    throw new Error(`${fixture}: rendered line for ${shareName} has no fsid=: ${line}`);
  }
  return { share, wantLine: line, fsid: fsidMatch[1] };
}

describe("buildNfsExportLine", () => {
  it("matches RenderNFSExports for IPv4 and IPv4 CIDR clients (nfs-hosts)", () => {
    const { share, wantLine, fsid } = loadShare("nfs-hosts", "media");
    const got = buildNfsExportLine("/mnt/user/media", {
      enabled: true,
      hosts: share.hosts,
      squash: share.squash,
      fsid,
    });
    expect(got).toBe(wantLine);
  });

  it("matches RenderNFSExports for IPv6 and IPv6 CIDR clients, bracketed (nfs-ipv6)", () => {
    const { share, wantLine, fsid } = loadShare("nfs-ipv6", "media");
    const got = buildNfsExportLine("/mnt/user/media", {
      enabled: true,
      hosts: share.hosts,
      squash: share.squash,
      fsid,
    });
    expect(got).toBe(wantLine);
  });

  it("matches RenderNFSExports for a non-canonical IPv6 CIDR address (nfs-ipv6-noncanonical)", () => {
    const { share, wantLine, fsid } = loadShare("nfs-ipv6-noncanonical", "media");
    const got = buildNfsExportLine("/mnt/user/media", {
      enabled: true,
      hosts: share.hosts,
      squash: share.squash,
      fsid,
    });
    expect(got).toBe(wantLine);
  });

  it("matches RenderNFSExports for a DNS hostname client (nfs-mixed, backup)", () => {
    const { share, wantLine, fsid } = loadShare("nfs-mixed", "backup");
    const got = buildNfsExportLine("/mnt/user/backup", {
      enabled: true,
      hosts: share.hosts,
      squash: share.squash,
      fsid,
    });
    expect(got).toBe(wantLine);
  });

  it("previews the bare path with no hosts", () => {
    expect(
      buildNfsExportLine("/mnt/user/media", {
        enabled: false,
        hosts: [],
        squash: "root_squash",
        fsid: "8eaf4655-d7ed-515a-89cb-bdb69f8209b3",
      }),
    ).toBe("/mnt/user/media");
  });

  it("previews the bare path when fsid is not yet known", () => {
    expect(
      buildNfsExportLine("/mnt/user/media", {
        enabled: true,
        hosts: ["10.0.0.5"],
        squash: "root_squash",
      }),
    ).toBe("/mnt/user/media");
  });
});
