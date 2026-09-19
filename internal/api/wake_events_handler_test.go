package api_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func TestHandler_ListWakeEvents_NilHistoryReturnsEmpty(t *testing.T) {
	h := &api.Handler{}
	resp, err := h.ListWakeEvents(context.Background())
	if err != nil {
		t.Fatalf("ListWakeEvents: %v", err)
	}
	if len(resp.Events) != 0 || len(resp.DailyWakeCounts) != 0 {
		t.Fatalf("expected empty response, got %d events and %d daily counts", len(resp.Events), len(resp.DailyWakeCounts))
	}
}

func newWakeEventsTestHandler(t *testing.T) (*api.Handler, *store.History) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "wake-events-handler-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	history := store.NewHistory(db)
	return &api.Handler{History: history}, history
}

func TestHandler_ListWakeEvents_FromPersistedRows(t *testing.T) {
	ctx := context.Background()
	h, history := newWakeEventsTestHandler(t)

	at := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	if err := history.RecordSpinEvent(ctx, "/dev/sdb", "standby", "active", at); err != nil {
		t.Fatalf("RecordSpinEvent: %v", err)
	}

	resp, err := h.ListWakeEvents(ctx)
	if err != nil {
		t.Fatalf("ListWakeEvents: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(resp.Events))
	}
	if resp.Events[0].Device != "/dev/sdb" {
		t.Fatalf("device = %q, want /dev/sdb", resp.Events[0].Device)
	}
	if resp.Events[0].FromState != apiv1.SpinTransitionFromStateStandby {
		t.Fatalf("fromState = %q, want standby", resp.Events[0].FromState)
	}
	if resp.Events[0].ToState != apiv1.SpinTransitionToStateActive {
		t.Fatalf("toState = %q, want active", resp.Events[0].ToState)
	}
	if len(resp.DailyWakeCounts) != 1 || resp.DailyWakeCounts[0].Count != 1 {
		t.Fatalf("daily wake counts = %+v, want one count of 1", resp.DailyWakeCounts)
	}
}
