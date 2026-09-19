package notify

import (
	"context"
	"testing"
)

func TestPublishPersistsInboxAlertAndBroadcasts(t *testing.T) {
	ctx := context.Background()
	hub := NewHub()
	svc := newTestService(t, nil)
	svc.Hub = hub

	ch, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	if err := svc.Publish(ctx, EventDiskOffline, "Disk offline", "sdb is offline"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case alert := <-ch:
		if alert.EventType != EventDiskOffline {
			t.Fatalf("hub alert event type = %q, want %q", alert.EventType, EventDiskOffline)
		}
	default:
		t.Fatal("expected hub to receive the persisted alert")
	}

	groups, unread, err := svc.ListInbox(ctx)
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if unread != 1 {
		t.Fatalf("unread = %d, want 1", unread)
	}
	if len(groups) != 1 || len(groups[0].Alerts) != 1 {
		t.Fatalf("groups = %#v, want one group with one alert", groups)
	}
}

func TestMarkReadAllClearsUnreadCount(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)

	if err := svc.Publish(ctx, EventDiskOffline, "Disk offline", "sdb is offline"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := svc.Publish(ctx, EventArrayDegraded, "Array degraded", "parity stale"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	unread, err := svc.MarkRead(ctx, nil, true)
	if err != nil {
		t.Fatalf("MarkRead all: %v", err)
	}
	if unread != 0 {
		t.Fatalf("unread after mark all = %d, want 0", unread)
	}
}
