import type { NotificationAlert, NotificationGroup } from "@/lib/api/events";

function prependAlert(groups: NotificationGroup[], alert: NotificationAlert): NotificationGroup[] {
  const next = groups.map((group) => ({ ...group, alerts: [...group.alerts] }));
  const index = next.findIndex((group) => group.eventType === alert.eventType);
  if (index >= 0) {
    const existing = next[index].alerts.filter((item) => item.id !== alert.id);
    next[index].alerts = [alert, ...existing];
    return next;
  }
  return [{ eventType: alert.eventType, alerts: [alert] }, ...next];
}

function flattenAlerts(groups: NotificationGroup[]): NotificationAlert[] {
  return groups.flatMap((group) => group.alerts);
}

export function notificationAlertIds(groups: NotificationGroup[]): Set<string> {
  return new Set(flattenAlerts(groups).map((alert) => alert.id));
}

export type NotificationInboxState = {
  groups: NotificationGroup[];
  unreadCount: number;
};

export type NotificationInboxAction =
  | { type: "snapshot"; groups: NotificationGroup[]; unreadCount: number }
  | { type: "alert"; alert: NotificationAlert }
  | { type: "markKnownRead"; knownIds: Set<string>; unreadCount: number };

export function notificationInboxReducer(
  state: NotificationInboxState,
  action: NotificationInboxAction,
): NotificationInboxState {
  switch (action.type) {
    case "snapshot": {
      const snapshotIds = notificationAlertIds(action.groups);
      const extras = flattenAlerts(state.groups).filter((alert) => !snapshotIds.has(alert.id));
      let groups = action.groups.map((group) => ({ ...group, alerts: [...group.alerts] }));
      for (const extra of extras) {
        groups = prependAlert(groups, extra);
      }
      return {
        groups,
        unreadCount: action.unreadCount + extras.filter((alert) => !alert.read).length,
      };
    }
    case "alert": {
      const alreadyPresent = state.groups.some((group) =>
        group.alerts.some((item) => item.id === action.alert.id),
      );
      return {
        groups: prependAlert(state.groups, action.alert),
        unreadCount: alreadyPresent ? state.unreadCount : state.unreadCount + 1,
      };
    }
    case "markKnownRead": {
      const extras = flattenAlerts(state.groups).filter((alert) => !action.knownIds.has(alert.id));
      return {
        groups: state.groups.map((group) => ({
          ...group,
          alerts: group.alerts.map((alert) =>
            action.knownIds.has(alert.id) ? { ...alert, read: true } : alert,
          ),
        })),
        unreadCount: action.unreadCount + extras.filter((alert) => !alert.read).length,
      };
    }
  }
}
