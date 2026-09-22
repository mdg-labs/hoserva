package notify

import (
	"context"
	"testing"
)

func TestPublishUPSOnBattery(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)

	if err := svc.PublishUPSOnBattery(ctx); err != nil {
		t.Fatalf("PublishUPSOnBattery: %v", err)
	}

	groups, unread, err := svc.ListInbox(ctx)
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if unread != 1 {
		t.Fatalf("unread = %d, want 1", unread)
	}
	if len(groups) != 1 || groups[0].EventType != EventUPSOnBattery {
		t.Fatalf("groups = %#v, want one EventUPSOnBattery group", groups)
	}
	if got, want := groups[0].Alerts[0].Severity, SeverityWarning; got != want {
		t.Fatalf("severity = %s, want the compiled-in default %s", got, want)
	}
}

func TestPublishUPSBatteryLow(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)

	if err := svc.PublishUPSBatteryLow(ctx); err != nil {
		t.Fatalf("PublishUPSBatteryLow: %v", err)
	}

	groups, unread, err := svc.ListInbox(ctx)
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if unread != 1 {
		t.Fatalf("unread = %d, want 1", unread)
	}
	if len(groups) != 1 || groups[0].EventType != EventUPSBatteryLow {
		t.Fatalf("groups = %#v, want one EventUPSBatteryLow group", groups)
	}
	if got, want := groups[0].Alerts[0].Severity, SeverityCritical; got != want {
		t.Fatalf("severity = %s, want the compiled-in default %s", got, want)
	}
}
