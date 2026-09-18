package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// newHistoryTestDB applies every real, embedded schema migration (D16) to
// a fresh database, so History's tests exercise spin_events and audit_log
// exactly as Runner leaves them at daemon startup, never a hand-built
// CREATE TABLE.
func newHistoryTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openTestDB(t)
	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	r := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := r.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return db
}

func TestHistory_RecordAndCountSpinEvents(t *testing.T) {
	ctx := context.Background()
	h := NewHistory(newHistoryTestDB(t))

	if err := h.RecordSpinEvent(ctx, "/dev/sdb", "active", "standby", time.Now()); err != nil {
		t.Fatalf("RecordSpinEvent: %v", err)
	}
	if err := h.RecordSpinEvent(ctx, "/dev/sdb", "standby", "active", time.Now()); err != nil {
		t.Fatalf("RecordSpinEvent: %v", err)
	}

	n, err := h.countSpinEvents(ctx)
	if err != nil {
		t.Fatalf("CountSpinEvents: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountSpinEvents = %d, want 2", n)
	}
}

func TestHistory_RecordSpinEvent_RejectsAnInvalidState(t *testing.T) {
	ctx := context.Background()
	h := NewHistory(newHistoryTestDB(t))

	if err := h.RecordSpinEvent(ctx, "/dev/sdb", "active", "sleeping", time.Now()); err == nil {
		t.Fatal("RecordSpinEvent: want an error for a to_state the schema's CHECK constraint doesn't allow, got nil")
	}
}

func TestHistory_RecordAndCountAuditLog(t *testing.T) {
	ctx := context.Background()
	h := NewHistory(newHistoryTestDB(t))

	if err := h.RecordAuditEntry(ctx, "admin", "disk.add", "/dev/sdc", time.Now()); err != nil {
		t.Fatalf("RecordAuditEntry: %v", err)
	}
	if err := h.RecordAuditEntry(ctx, "admin", "share.create", "", time.Now()); err != nil {
		t.Fatalf("RecordAuditEntry with no detail: %v", err)
	}

	n, err := h.countAuditLog(ctx)
	if err != nil {
		t.Fatalf("CountAuditLog: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountAuditLog = %d, want 2", n)
	}
}

func TestHistory_PruneHistory_DeletesOnlyOlderThanTwoYears(t *testing.T) {
	ctx := context.Background()
	h := NewHistory(newHistoryTestDB(t))
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	old := now.Add(-HistoryRetention).Add(-time.Hour)
	recent := now.Add(-HistoryRetention).Add(time.Hour)

	if err := h.RecordSpinEvent(ctx, "/dev/sdb", "active", "standby", old); err != nil {
		t.Fatalf("RecordSpinEvent(old): %v", err)
	}
	if err := h.RecordSpinEvent(ctx, "/dev/sdb", "standby", "active", recent); err != nil {
		t.Fatalf("RecordSpinEvent(recent): %v", err)
	}
	if err := h.RecordAuditEntry(ctx, "admin", "disk.add", "", old); err != nil {
		t.Fatalf("RecordAuditEntry(old): %v", err)
	}
	if err := h.RecordAuditEntry(ctx, "admin", "disk.remove", "", recent); err != nil {
		t.Fatalf("RecordAuditEntry(recent): %v", err)
	}

	if err := h.PruneHistory(ctx, now); err != nil {
		t.Fatalf("PruneHistory: %v", err)
	}

	if n, err := h.countSpinEvents(ctx); err != nil || n != 1 {
		t.Fatalf("CountSpinEvents after prune = %d, %v, want 1, nil", n, err)
	}
	if n, err := h.countAuditLog(ctx); err != nil || n != 1 {
		t.Fatalf("CountAuditLog after prune = %d, %v, want 1, nil", n, err)
	}
}
