package notify

import (
	"context"
	"strings"
	"testing"
)

func TestPublishDiskNearMinFreeSpace(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)

	if err := svc.PublishDiskNearMinFreeSpace(ctx, "/mnt/disk2", 2*(1<<30), 1000*(1<<30)); err != nil {
		t.Fatalf("PublishDiskNearMinFreeSpace: %v", err)
	}

	groups, unread, err := svc.ListInbox(ctx)
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if unread != 1 {
		t.Fatalf("unread = %d, want 1", unread)
	}
	if len(groups) != 1 || groups[0].EventType != EventDiskNearMinFreeSpace {
		t.Fatalf("groups = %#v, want one EventDiskNearMinFreeSpace group", groups)
	}
	alert := groups[0].Alerts[0]
	if !strings.Contains(alert.Title, "/mnt/disk2") {
		t.Fatalf("alert title = %q, want it to name the constrained disk", alert.Title)
	}
	if !strings.Contains(alert.Message, "2 GiB") || !strings.Contains(alert.Message, "1000 GiB") {
		t.Fatalf("alert message = %q, want it to report free and total space", alert.Message)
	}
}

func TestPublishRebalanceSuggested(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)

	reason := "/mnt/disk1 is at or below minfreespace; /mnt/disk3 still has room"
	if err := svc.PublishRebalanceSuggested(ctx, "/mnt/disk1", reason); err != nil {
		t.Fatalf("PublishRebalanceSuggested: %v", err)
	}

	groups, _, err := svc.ListInbox(ctx)
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(groups) != 1 || groups[0].EventType != EventDiskNearMinFreeSpace {
		t.Fatalf("groups = %#v, want one EventDiskNearMinFreeSpace group", groups)
	}
	alert := groups[0].Alerts[0]
	if !strings.Contains(alert.Title, "/mnt/disk1") {
		t.Fatalf("alert title = %q, want it to name the constrained disk", alert.Title)
	}
	if alert.Message != reason {
		t.Fatalf("alert message = %q, want the given reason unchanged", alert.Message)
	}
}

func TestFormatGiB(t *testing.T) {
	if got := formatGiB(0); got != "0 GiB" {
		t.Fatalf("formatGiB(0) = %q, want %q", got, "0 GiB")
	}
	if got := formatGiB(5 * (1 << 30)); got != "5 GiB" {
		t.Fatalf("formatGiB(5 GiB) = %q, want %q", got, "5 GiB")
	}
}
