import { describe, expect, it } from "vitest";

import { notificationInboxReducer } from "@/components/patterns/notification-inbox";
import type { NotificationAlert, NotificationGroup } from "@/lib/api/events";

function alert(overrides: Partial<NotificationAlert> & Pick<NotificationAlert, "id">): NotificationAlert {
  return {
    eventType: "smart_warning",
    level: "warning",
    title: "SMART warning",
    message: "Reallocated sectors rising",
    createdAt: "2026-09-19T20:00:00Z",
    read: false,
    ...overrides,
  };
}

function group(alerts: NotificationAlert[]): NotificationGroup {
  return { eventType: alerts[0].eventType, alerts };
}

describe("notificationInboxReducer", () => {
  it("keeps an SSE alert that arrived while a snapshot request was in flight", () => {
    const live = alert({ id: "live" });
    const afterLive = notificationInboxReducer(
      { groups: [], unreadCount: 0 },
      { type: "alert", alert: live },
    );
    const snapshot = group([alert({ id: "persisted" })]);
    const merged = notificationInboxReducer(afterLive, {
      type: "snapshot",
      groups: [snapshot],
      unreadCount: 1,
    });

    const ids = merged.groups.flatMap((item) => item.alerts.map((a) => a.id));
    expect(ids).toEqual(["live", "persisted"]);
    expect(merged.unreadCount).toBe(2);
  });

  it("does not mark a post-operation SSE alert read when mark-all returns", () => {
    const known = alert({ id: "known" });
    const later = alert({ id: "later" });
    const withBoth = notificationInboxReducer(
      { groups: [group([known])], unreadCount: 1 },
      { type: "alert", alert: later },
    );

    const marked = notificationInboxReducer(withBoth, {
      type: "markKnownRead",
      knownIds: new Set(["known"]),
      unreadCount: 0,
    });

    const byId = Object.fromEntries(
      marked.groups.flatMap((item) => item.alerts.map((a) => [a.id, a.read])),
    );
    expect(byId.known).toBe(true);
    expect(byId.later).toBe(false);
    expect(marked.unreadCount).toBe(1);
  });
});
