import { describe, expect, it } from "vitest";

import {
  appComposePath,
  appDetailPath,
  canRemove,
  canStart,
  canStop,
  durationParts,
  publishedPorts,
  removeRefusalKey,
  updateSummary,
  type AppPort,
  type AppUpdate,
} from "@/routes/apps/containers";

describe("canStart and canStop", () => {
  it("offer start for a stopped container and stop for a live one, never both", () => {
    expect(canStart("exited")).toBe(true);
    expect(canStart("created")).toBe(true);
    expect(canStop("running")).toBe(true);
    expect(canStop("restarting")).toBe(true);
    expect(canStop("paused")).toBe(true);
    for (const state of ["created", "running", "paused", "restarting", "removing", "exited", "dead"] as const) {
      expect(canStart(state) && canStop(state)).toBe(false);
    }
  });

  it("offers neither for a container the Engine is removing or has marked dead", () => {
    for (const state of ["removing", "dead"] as const) {
      expect(canStart(state)).toBe(false);
      expect(canStop(state)).toBe(false);
    }
  });
});

describe("publishedPorts", () => {
  it("links a wildcard binding through the address the page was opened on", () => {
    const ports: AppPort[] = [{ hostIP: "0.0.0.0", hostPort: 8096, containerPort: 8096, protocol: "tcp" }];
    expect(publishedPorts(ports, "nas.local")).toEqual([
      { key: "8096/tcp", port: 8096, protocol: "tcp", href: "http://nas.local:8096" },
    ]);
  });

  it("lists the IPv4 and IPv6 bindings of one host port once", () => {
    const ports: AppPort[] = [
      { hostIP: "0.0.0.0", hostPort: 8080, containerPort: 80, protocol: "tcp" },
      { hostIP: "::", hostPort: 8080, containerPort: 80, protocol: "tcp" },
    ];
    expect(publishedPorts(ports, "nas.local")).toHaveLength(1);
  });

  it("links a LAN binding of a port even when a loopback binding of it is listed first", () => {
    const ports: AppPort[] = [
      { hostIP: "127.0.0.1", hostPort: 8080, containerPort: 80, protocol: "tcp" },
      { hostIP: "0.0.0.0", hostPort: 8080, containerPort: 80, protocol: "tcp" },
    ];
    expect(publishedPorts(ports, "nas.local")).toEqual([
      { key: "8080/tcp", port: 8080, protocol: "tcp", href: "http://nas.local:8080" },
    ]);
  });

  it("keeps the first reachable binding when several are", () => {
    const ports: AppPort[] = [
      { hostIP: "192.168.1.5", hostPort: 8080, containerPort: 80, protocol: "tcp" },
      { hostIP: "0.0.0.0", hostPort: 8080, containerPort: 80, protocol: "tcp" },
    ];
    expect(publishedPorts(ports, "nas.local").map((port) => port.href)).toEqual(["http://192.168.1.5:8080"]);
  });

  it("skips a container port that is not published to the host", () => {
    expect(publishedPorts([{ containerPort: 5432, protocol: "tcp" }], "nas.local")).toEqual([]);
  });

  it("does not link a loopback binding or a non-TCP port", () => {
    const ports: AppPort[] = [
      { hostIP: "127.0.0.1", hostPort: 9000, containerPort: 9000, protocol: "tcp" },
      { hostIP: "0.0.0.0", hostPort: 53, containerPort: 53, protocol: "udp" },
    ];
    expect(publishedPorts(ports, "nas.local").map((port) => port.href)).toEqual([null, null]);
  });

  it("links a binding to a specific address through that address, bracketing IPv6", () => {
    const ports: AppPort[] = [
      { hostIP: "192.168.1.5", hostPort: 8000, containerPort: 80, protocol: "tcp" },
      { hostIP: "fd00::5", hostPort: 8001, containerPort: 80, protocol: "tcp" },
    ];
    expect(publishedPorts(ports, "nas.local").map((port) => port.href)).toEqual([
      "http://192.168.1.5:8000",
      "http://[fd00::5]:8001",
    ]);
  });
});

describe("updateSummary", () => {
  const base = { container: "app", image: "img", tag: "1" };

  it("reports an update only for update_available", () => {
    const update: AppUpdate = { ...base, status: "update_available", kind: "new_version", availableTag: "2" };
    expect(updateSummary(update)).toEqual({ kind: "available", change: "new_version", tag: "2" });
  });

  it("shows nothing for an image that is up to date", () => {
    expect(updateSummary({ ...base, status: "up_to_date" })).toBeNull();
  });

  it("never reads skipped, failed or not_checked as up to date", () => {
    for (const status of ["skipped", "failed", "not_checked"] as const) {
      expect(updateSummary({ ...base, status, message: "why" })).toEqual({ kind: "unchecked", message: "why" });
    }
  });

  it("treats a container the check did not list as not checked", () => {
    expect(updateSummary(undefined)).toEqual({ kind: "unchecked" });
  });
});

describe("appDetailPath", () => {
  it("escapes the container name", () => {
    expect(appDetailPath("my app/1")).toBe("/apps/my%20app%2F1");
  });
});

describe("canRemove", () => {
  it("allows a container that is not running and none that is live or being removed", () => {
    for (const state of ["created", "exited", "dead"] as const) {
      expect(canRemove(state)).toBe(true);
    }
    for (const state of ["running", "paused", "restarting", "removing"] as const) {
      expect(canRemove(state)).toBe(false);
    }
  });
});

describe("appComposePath", () => {
  it("is the container's page followed by compose", () => {
    expect(appComposePath("jelly fin")).toBe("/apps/jelly%20fin/compose");
  });
});

describe("durationParts", () => {
  const minute = 60_000;
  it("keeps the two largest non-zero units", () => {
    expect(durationParts(3 * 60 * minute + 30 * minute)).toEqual([
      { unit: "hour", count: 3 },
      { unit: "minute", count: 30 },
    ]);
    expect(durationParts((2 * 24 * 60 + 5 * 60 + 7) * minute)).toEqual([
      { unit: "day", count: 2 },
      { unit: "hour", count: 5 },
    ]);
    expect(durationParts(24 * 60 * minute + 7 * minute)).toEqual([
      { unit: "day", count: 1 },
      { unit: "minute", count: 7 },
    ]);
  });

  it("has no part for less than a minute", () => {
    expect(durationParts(59_999)).toEqual([]);
    expect(durationParts(0)).toEqual([]);
  });
});

describe("removeRefusalKey", () => {
  it("names the five refusals the page explains and nothing else", () => {
    for (const code of ["app_running", "appdata_shared", "appdata_unavailable", "array_stopped", "stack_project_shared"]) {
      expect(removeRefusalKey(code)).toBe(code);
    }
    expect(removeRefusalKey("app_action_failed")).toBeNull();
    expect(removeRefusalKey(undefined)).toBeNull();
  });
});
