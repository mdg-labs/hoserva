package main

import (
	"context"
	"testing"
)

func TestListWakeEvents_HealthyScenarioIsNonEmpty(t *testing.T) {
	client := newTestClient(t, "healthy")

	resp, err := client.ListWakeEvents(context.Background())
	if err != nil {
		t.Fatalf("ListWakeEvents: %v", err)
	}
	if len(resp.Events) == 0 {
		t.Fatal("healthy scenario should serve a non-empty wake-events timeline")
	}
	if len(resp.DailyWakeCounts) == 0 {
		t.Fatal("healthy scenario should serve daily wake counts")
	}
}

func TestListWakeEvents_FreshInstallIsEmpty(t *testing.T) {
	client := newTestClient(t, "fresh-install")

	resp, err := client.ListWakeEvents(context.Background())
	if err != nil {
		t.Fatalf("ListWakeEvents: %v", err)
	}
	if len(resp.Events) != 0 || len(resp.DailyWakeCounts) != 0 {
		t.Fatalf("fresh-install should be empty, got %d events and %d daily counts", len(resp.Events), len(resp.DailyWakeCounts))
	}
}
