import type { StatusTone } from "@/components/patterns/status-badge";
import type { components } from "@/lib/api/client";

export type App = components["schemas"]["App"];
export type AppPort = components["schemas"]["AppPort"];
export type AppState = components["schemas"]["AppState"];
export type AppUpdate = components["schemas"]["AppUpdate"];
export type LifecycleAction = "start" | "stop" | "restart";
export const START = "start" as const;
export const STOP = "stop" as const;
export const RESTART = "restart" as const;

export function appDetailPath(name: string): string {
  return `/apps/${encodeURIComponent(name)}`;
}

// The Engine refuses to start a paused or dead container and to stop one that
// is not running, so each action is offered only where it can work.
export function canStart(state: AppState): boolean {
  return state === "created" || state === "exited";
}

export function canStop(state: AppState): boolean {
  return state === "running" || state === "restarting" || state === "paused";
}

export function stateTone(state: AppState): StatusTone {
  switch (state) {
    case "running":
      return "success";
    case "paused":
    case "restarting":
    case "removing":
      return "warning";
    case "dead":
      return "error";
    default:
      return "outline";
  }
}

export type PublishedPort = {
  key: string;
  port: number;
  protocol: AppPort["protocol"];
  href: string | null;
};

const WILDCARD_HOSTS = new Set(["", "0.0.0.0", "::", "[::]"]);

function isLoopback(host: string): boolean {
  return host === "::1" || host === "[::1]" || host.startsWith("127.");
}

function bracketed(host: string): string {
  return host.includes(":") && !host.startsWith("[") ? `[${host}]` : host;
}

// A port bound to the loopback address cannot be reached from the browser, so
// it is listed without a link. A wildcard binding is reached through the
// address the page itself was opened on.
function portHref(port: AppPort, pageHostname: string): string | null {
  if (port.protocol !== "tcp" || port.hostPort === undefined) {
    return null;
  }
  const bound = port.hostIP ?? "";
  if (isLoopback(bound)) {
    return null;
  }
  const host = WILDCARD_HOSTS.has(bound) ? pageHostname : bracketed(bound);
  return `http://${host}:${port.hostPort}`;
}

// A container publishes each port once per address family, so IPv4 and IPv6
// bindings of the same host port are one entry.
export function publishedPorts(ports: AppPort[], pageHostname: string): PublishedPort[] {
  const seen = new Map<string, PublishedPort>();
  for (const port of ports) {
    if (port.hostPort === undefined) {
      continue;
    }
    const key = `${port.hostPort}/${port.protocol}`;
    if (!seen.has(key)) {
      seen.set(key, { key, port: port.hostPort, protocol: port.protocol, href: portHref(port, pageHostname) });
    }
  }
  return [...seen.values()];
}

export type UpdateSummary =
  | { kind: "available"; change?: "new_build" | "new_version"; tag?: string }
  | { kind: "unchecked"; message?: string };

// Only update_available reads as an update. skipped, failed and not_checked
// are never reported as up to date, and a container the check did not list at
// all is treated the same way.
export function updateSummary(update: AppUpdate | undefined): UpdateSummary | null {
  if (update === undefined) {
    return { kind: "unchecked" };
  }
  switch (update.status) {
    case "up_to_date":
      return null;
    case "update_available":
      return { kind: "available", change: update.kind, tag: update.availableTag };
    default:
      return { kind: "unchecked", message: update.message };
  }
}
