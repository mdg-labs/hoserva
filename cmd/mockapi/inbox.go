package main

import (
	"context"
	"sort"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/notify"
)

func defaultInboxAlerts(scenario string) []apiv1.NotificationAlert {
	now := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	switch scenario {
	case "degraded":
		return []apiv1.NotificationAlert{
			{
				ID:        "n-degraded-1",
				EventType: apiv1.NotificationEventTypeArrayDegraded,
				Level:     apiv1.NotificationLevelError,
				Title:     "Array degraded",
				Message:   "Parity check found mismatched blocks on disk2 — sync to clear.",
				CreatedAt: now.Add(-2 * time.Hour),
				Read:      false,
			},
			{
				ID:        "n-degraded-2",
				EventType: apiv1.NotificationEventTypeSyncBlockedThreshold,
				Level:     apiv1.NotificationLevelCritical,
				Title:     "Sync blocked",
				Message:   "The threshold guard is tripped on disk3.",
				CreatedAt: now.Add(-30 * time.Minute),
				Read:      false,
			},
		}
	case "sync-blocked":
		return []apiv1.NotificationAlert{
			{
				ID:        "n-sync-blocked-1",
				EventType: apiv1.NotificationEventTypeSyncBlockedThreshold,
				Level:     apiv1.NotificationLevelWarning,
				Title:     "Sync blocked",
				Message:   "The free-space threshold guard is tripped on disk3 — sync refused until it clears.",
				CreatedAt: now.Add(-6 * time.Hour),
				Read:      false,
			},
		}
	case "fresh-install":
		return []apiv1.NotificationAlert{
			{
				ID:        "n-fresh-install-1",
				EventType: apiv1.NotificationEventTypeSyncSucceeded,
				Level:     apiv1.NotificationLevelInfo,
				Title:     "Fresh install",
				Message:   "No jobs yet — the array is empty and ready to configure.",
				CreatedAt: now,
				Read:      false,
			},
		}
	default:
		return nil
	}
}

func groupMockAlerts(alerts []apiv1.NotificationAlert) []apiv1.NotificationGroup {
	byType := make(map[apiv1.NotificationEventType][]apiv1.NotificationAlert)
	for _, a := range alerts {
		byType[a.EventType] = append(byType[a.EventType], a)
	}
	out := make([]apiv1.NotificationGroup, 0)
	for _, event := range notify.EventCatalog {
		items := byType[apiv1.NotificationEventType(event)]
		if len(items) == 0 {
			continue
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Read != items[j].Read {
				return !items[i].Read
			}
			return items[i].CreatedAt.After(items[j].CreatedAt)
		})
		out = append(out, apiv1.NotificationGroup{EventType: apiv1.NotificationEventType(event), Alerts: items})
	}
	return out
}

func countUnreadAlerts(alerts []apiv1.NotificationAlert) int {
	n := 0
	for _, a := range alerts {
		if !a.Read {
			n++
		}
	}
	return n
}

func (h *handler) ListNotifications(ctx context.Context) (*apiv1.ListNotificationsOK, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	groups := groupMockAlerts(h.inboxAlerts)
	return &apiv1.ListNotificationsOK{
		Groups:      groups,
		UnreadCount: countUnreadAlerts(h.inboxAlerts),
	}, nil
}

func (h *handler) MarkNotificationsRead(ctx context.Context, req *apiv1.MarkNotificationsReadRequest) (*apiv1.MarkNotificationsReadOK, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	all := false
	if v, ok := req.All.Get(); ok {
		all = v
	}
	if all {
		for i := range h.inboxAlerts {
			h.inboxAlerts[i].Read = true
		}
	} else if len(req.Ids) > 0 {
		want := make(map[string]bool, len(req.Ids))
		for _, id := range req.Ids {
			want[id] = true
		}
		for i := range h.inboxAlerts {
			if want[h.inboxAlerts[i].ID] {
				h.inboxAlerts[i].Read = true
			}
		}
	}
	return &apiv1.MarkNotificationsReadOK{UnreadCount: countUnreadAlerts(h.inboxAlerts)}, nil
}
