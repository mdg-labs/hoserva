import type { StatusTone } from "@/components/patterns/status-badge";
import type { components } from "@/lib/api/client";

export type App = components["schemas"]["App"];
export type AppPort = components["schemas"]["AppPort"];
export type AppState = components["schemas"]["AppState"];
export type AppMountLocation = components["schemas"]["AppMountLocation"];
export type AppUpdate = components["schemas"]["AppUpdate"];
export type LifecycleAction = "start" | "stop" | "restart";
export const START = "start" as const;
export const STOP = "stop" as const;
export const RESTART = "restart" as const;

export function appDetailPath(name: string): string {
  return `/apps/${encodeURIComponent(name)}`;
}

export function appComposePath(name: string): string {
  return `${appDetailPath(name)}/compose`;
}

// The Engine refuses to start a paused or dead container and to stop one that
// is not running, so each action is offered only where it can work.
export function canStart(state: AppState): boolean {
  return state === "created" || state === "exited";
}

export function canStop(state: AppState): boolean {
  return state === "running" || state === "restarting" || state === "paused";
}

// A container that is still running is never removed by itself: the daemon
// refuses it (app_running), so the action is offered only where it can work.
// A stack's containers are the exception, since removing the stack takes them
// down first.
export function canRemove(state: AppState): boolean {
  return state === "created" || state === "exited" || state === "dead";
}

export type DurationPart = { unit: "day" | "hour" | "minute"; count: number };

const MINUTE_MS = 60_000;
const HOUR_MS = 60 * MINUTE_MS;
const DAY_MS = 24 * HOUR_MS;

// The two largest non-zero units of a duration, or none for less than a
// minute.
export function durationParts(ms: number): DurationPart[] {
  const days = Math.floor(ms / DAY_MS);
  const hours = Math.floor((ms % DAY_MS) / HOUR_MS);
  const minutes = Math.floor((ms % HOUR_MS) / MINUTE_MS);
  const parts: DurationPart[] = [
    { unit: "day", count: days },
    { unit: "hour", count: hours },
    { unit: "minute", count: minutes },
  ];
  return parts.filter((part) => part.count > 0).slice(0, 2);
}

// The refusals the daemon gives a removal that the page explains in its own
// words; any other code falls back to the server's message alone.
const REMOVE_REFUSALS = new Set([
  "app_running",
  "appdata_shared",
  "appdata_unavailable",
  "array_stopped",
  "stack_project_shared",
]);

export function removeRefusalKey(code: string | undefined): string | null {
  return code !== undefined && REMOVE_REFUSALS.has(code) ? code : null;
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
// bindings of the same host port are one entry. When one binding of a port is
// reachable from the browser and another is loopback-only, the entry links to
// the reachable one, whichever the Engine listed first.
export function publishedPorts(ports: AppPort[], pageHostname: string): PublishedPort[] {
  const seen = new Map<string, PublishedPort>();
  for (const port of ports) {
    if (port.hostPort === undefined) {
      continue;
    }
    const key = `${port.hostPort}/${port.protocol}`;
    const href = portHref(port, pageHostname);
    const existing = seen.get(key);
    if (existing === undefined) {
      seen.set(key, { key, port: port.hostPort, protocol: port.protocol, href });
    } else if (existing.href === null && href !== null) {
      existing.href = href;
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
