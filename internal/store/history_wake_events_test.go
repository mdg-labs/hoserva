package store

import (
	"context"
	"testing"
	"time"
)

func TestHistory_ListWakeEvents_ReturnsTransitionsAndDailyCounts(t *testing.T) {
	ctx := context.Background()
	h := NewHistory(newHistoryTestDB(t))

	day := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	wake1 := day.Add(8 * time.Hour)
	standby1 := wake1.Add(30 * time.Minute)
	wake2 := day.Add(14 * time.Hour)

	if err := h.RecordSpinEvent(ctx, "/dev/sdb", "standby", "active", wake1); err != nil {
		t.Fatalf("RecordSpinEvent(wake1): %v", err)
	}
	if err := h.RecordSpinEvent(ctx, "/dev/sdb", "active", "standby", standby1); err != nil {
		t.Fatalf("RecordSpinEvent(standby1): %v", err)
	}
	if err := h.RecordSpinEvent(ctx, "/dev/sdb", "standby", "active", wake2); err != nil {
		t.Fatalf("RecordSpinEvent(wake2): %v", err)
	}
	if err := h.RecordSpinEvent(ctx, "/dev/sdc", "standby", "active", day.Add(10*time.Hour)); err != nil {
		t.Fatalf("RecordSpinEvent(sdc): %v", err)
	}

	events, daily, err := h.ListWakeEvents(ctx)
	if err != nil {
		t.Fatalf("ListWakeEvents: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("len(events) = %d, want 4", len(events))
	}
	if events[0].At != wake2 {
		t.Fatalf("events[0].At = %v, want newest first %v", events[0].At, wake2)
	}

	var wake1Event *ListedSpinEvent
	for i := range events {
		if events[i].At.Equal(wake1) {
			wake1Event = &events[i]
			break
		}
	}
	if wake1Event == nil {
		t.Fatal("wake1 event not found")
	}
	if wake1Event.AwakeDurationSeconds == nil {
		t.Fatal("wake1 awake duration missing")
	}
	if *wake1Event.AwakeDurationSeconds != 30*60 {
		t.Fatalf("wake1 awake duration = %d, want %d", *wake1Event.AwakeDurationSeconds, 30*60)
	}

	if len(daily) != 2 {
		t.Fatalf("len(daily) = %d, want 2", len(daily))
	}
	var sdbCount int32
	for _, row := range daily {
		if row.Device == "/dev/sdb" && row.Date.Equal(day) {
			sdbCount = row.Count
		}
	}
	if sdbCount != 2 {
		t.Fatalf("/dev/sdb wake count = %d, want 2", sdbCount)
	}
}

func TestHistory_ListWakeEvents_Empty(t *testing.T) {
	ctx := context.Background()
	h := NewHistory(newHistoryTestDB(t))

	events, daily, err := h.ListWakeEvents(ctx)
	if err != nil {
		t.Fatalf("ListWakeEvents: %v", err)
	}
	if len(events) != 0 || len(daily) != 0 {
		t.Fatalf("expected empty lists, got %d events and %d daily counts", len(events), len(daily))
	}
}
