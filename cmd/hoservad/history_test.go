package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

const historyFixtureDir = "../../testdata/parsers"

func newHistoryTestDB(t *testing.T) *sql.DB {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-history-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return db
}

func readHistoryFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(historyFixtureDir, name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func newHistoryTestLinuxProvider(t *testing.T) (*disk.LinuxProvider, *disk.FakeRunner) {
	t.Helper()
	runner := disk.NewFakeRunner()
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	return &disk.LinuxProvider{
		Exec:   runner,
		Events: disk.NewSpinEventLog(),
		Trends: disk.NewTrendTracker(),
		Now:    func() time.Time { return now },
	}, runner
}

// TestWireDaemonHistory_SetsHandlerHistory is the construction half of
// #192: store.History from the daemon database must be injected on
// api.Handler so GET /disks/wake-events reads persisted rows.
func TestWireDaemonHistory_SetsHandlerHistory(t *testing.T) {
	db := newHistoryTestDB(t)
	h := &api.Handler{}

	history := wireDaemonHistory(db, h)
	if history == nil {
		t.Fatal("wireDaemonHistory returned nil")
	}
	if h.History != history {
		t.Fatal("handler.History is not the wired store.History")
	}
}

// TestPersistingDiskProvider_PersistsSpinTransitionOnSMART is the
// observe-and-persist half of #192: a transition LinuxProvider already
// records in SpinEventLog from a respectful SMART poll must land in
// SQLite and come back through ListWakeEvents — no real block devices.
func TestPersistingDiskProvider_PersistsSpinTransitionOnSMART(t *testing.T) {
	ctx := context.Background()
	db := newHistoryTestDB(t)
	h := &api.Handler{}
	history := wireDaemonHistory(db, h)

	linux, runner := newHistoryTestLinuxProvider(t)
	provider := newPersistingDiskProvider(linux, history)
	h.Disks = provider

	standby := readHistoryFixture(t, "smartctl_ata_standby.json")
	healthy := readHistoryFixture(t, "smartctl_ata_healthy.json")
	runner.Script("smartctl", []string{"-j", "-n", "standby", "-a", "/dev/sdb"}, standby, nil)
	if _, err := provider.SMART(ctx, "/dev/sdb", disk.SMARTPollRespectStandby); err != nil {
		t.Fatalf("SMART (standby): %v", err)
	}

	runner.Script("smartctl", []string{"-j", "-n", "standby", "-a", "/dev/sdb"}, healthy, nil)
	if _, err := provider.SMART(ctx, "/dev/sdb", disk.SMARTPollRespectStandby); err != nil {
		t.Fatalf("SMART (healthy): %v", err)
	}

	resp, err := h.ListWakeEvents(ctx)
	if err != nil {
		t.Fatalf("ListWakeEvents: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("len(events) = %d, want 1 persisted transition", len(resp.Events))
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

// TestPruneOnce_PrunesOldSpinEvents confirms the existing daily retention
// loop also calls History.PruneHistory (Q74) — spin events older than two
// years are deleted, recent rows stay.
func TestPruneOnce_PrunesOldSpinEvents(t *testing.T) {
	ctx := context.Background()
	db := newHistoryTestDB(t)
	history := store.NewHistory(db)

	now := time.Now()
	old := now.Add(-store.HistoryRetention).Add(-time.Hour)
	recent := now.Add(-store.HistoryRetention).Add(time.Hour)
	if err := history.RecordSpinEvent(ctx, "/dev/sdb", "active", "standby", old); err != nil {
		t.Fatalf("RecordSpinEvent(old): %v", err)
	}
	if err := history.RecordSpinEvent(ctx, "/dev/sdb", "standby", "active", recent); err != nil {
		t.Fatalf("RecordSpinEvent(recent): %v", err)
	}

	jobStore := job.NewStore(db)
	logs := job.NewLogStore(t.TempDir())
	authStore := api.NewAuthStore(db)
	pruneOnce(ctx, jobStore, logs, authStore, history)

	events, _, err := history.ListWakeEvents(ctx)
	if err != nil {
		t.Fatalf("ListWakeEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) after prune = %d, want 1 recent row kept", len(events))
	}
}
