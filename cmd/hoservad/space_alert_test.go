package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// fakeSpaceAlertNotifier records every publish call this runner makes,
// standing in for a real notify.Service the same way fakeThresholdStatter
// (mover_threshold_test.go) stands in for pool.StatfsSpaceStatter.
type fakeSpaceAlertNotifier struct {
	nearCalls      []string
	rebalanceCalls []string
}

func (f *fakeSpaceAlertNotifier) PublishDiskNearMinFreeSpace(_ context.Context, diskPath string, _, _ int64) error {
	f.nearCalls = append(f.nearCalls, diskPath)
	return nil
}

func (f *fakeSpaceAlertNotifier) PublishRebalanceSuggested(_ context.Context, constrainedDiskPath, _ string) error {
	f.rebalanceCalls = append(f.rebalanceCalls, constrainedDiskPath)
	return nil
}

func newSpaceAlertTestArrayStore(t *testing.T) *store.ArrayStore {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-space-alert-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return store.NewArrayStore(db)
}

func putSpaceAlertTestArray(t *testing.T, arrays *store.ArrayStore, createPolicy, minFreeSpace string, disks []store.ArrayDisk) {
	t.Helper()
	if err := arrays.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: createPolicy,
		MinFreeSpace: minFreeSpace,
		CreatedAt:    time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
	}, disks); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
}

// TestSpaceAlertRunner_FiresOnceOnNearMinFreeSpaceTransition proves the
// acceptance criterion directly: the same near-threshold disk reported
// across two consecutive ticks publishes exactly once, not once per tick.
func TestSpaceAlertRunner_FiresOnceOnNearMinFreeSpaceTransition(t *testing.T) {
	arrays := newSpaceAlertTestArrayStore(t)
	putSpaceAlertTestArray(t, arrays, "mfs", "20G", []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Serial: "DATA1", ByIDName: "wwn-wwn-d1", Mountpoint: "/mnt/disk1"},
	})
	notifier := &fakeSpaceAlertNotifier{}
	r := &spaceAlertRunner{
		Array: arrays,
		Statter: fakeThresholdStatter{stats: map[string]pool.SpaceStat{
			"/mnt/disk1": {TotalBytes: 100 << 30, FreeBytes: 10 << 30}, // below 20G minfreespace
		}},
		Notifier: notifier,
	}

	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}

	if len(notifier.nearCalls) != 1 {
		t.Fatalf("nearCalls = %v, want exactly one publish across two ticks at the same threshold", notifier.nearCalls)
	}
}

// TestSpaceAlertRunner_RefiresAfterRecovery proves the runner clears its
// per-disk state once a disk recovers above minfreespace, so a later
// re-crossing fires again rather than staying silently suppressed.
func TestSpaceAlertRunner_RefiresAfterRecovery(t *testing.T) {
	arrays := newSpaceAlertTestArrayStore(t)
	putSpaceAlertTestArray(t, arrays, "mfs", "20G", []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Serial: "DATA1", ByIDName: "wwn-wwn-d1", Mountpoint: "/mnt/disk1"},
	})
	notifier := &fakeSpaceAlertNotifier{}
	statter := fakeThresholdStatter{stats: map[string]pool.SpaceStat{
		"/mnt/disk1": {TotalBytes: 100 << 30, FreeBytes: 10 << 30},
	}}
	r := &spaceAlertRunner{Array: arrays, Statter: statter, Notifier: notifier}

	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}

	r.Statter = fakeThresholdStatter{stats: map[string]pool.SpaceStat{
		"/mnt/disk1": {TotalBytes: 100 << 30, FreeBytes: 50 << 30}, // recovered
	}}
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("recovery tick: %v", err)
	}

	r.Statter = statter // back below the threshold
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("re-crossing tick: %v", err)
	}

	if len(notifier.nearCalls) != 2 {
		t.Fatalf("nearCalls = %v, want two publishes: once on the first crossing, once on the re-crossing after recovery", notifier.nearCalls)
	}
}

// TestSpaceAlertRunner_RebalanceSuggestedFiresOnceOnTransition proves the
// same no-repeat-per-tick behaviour for PublishRebalanceSuggested: with
// KeepFoldersTogether and one disk constrained while another still has
// room, two consecutive ticks reporting the same suggestion publish once.
func TestSpaceAlertRunner_RebalanceSuggestedFiresOnceOnTransition(t *testing.T) {
	arrays := newSpaceAlertTestArrayStore(t)
	putSpaceAlertTestArray(t, arrays, "mspmfs", "20G", []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Serial: "DATA1", ByIDName: "wwn-wwn-d1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", WWN: "wwn-d2", Serial: "DATA2", ByIDName: "wwn-wwn-d2", Mountpoint: "/mnt/disk2"},
	})
	notifier := &fakeSpaceAlertNotifier{}
	r := &spaceAlertRunner{
		Array: arrays,
		Statter: fakeThresholdStatter{stats: map[string]pool.SpaceStat{
			"/mnt/disk1": {TotalBytes: 100 << 30, FreeBytes: 10 << 30}, // constrained
			"/mnt/disk2": {TotalBytes: 100 << 30, FreeBytes: 80 << 30}, // has room
		}},
		Notifier: notifier,
	}

	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}

	if len(notifier.rebalanceCalls) != 1 || notifier.rebalanceCalls[0] != "/mnt/disk1" {
		t.Fatalf("rebalanceCalls = %v, want exactly one publish for /mnt/disk1 across two ticks", notifier.rebalanceCalls)
	}
}

// TestSpaceAlertRunner_NoArrayYetIsNoOp proves the check never blocks the
// schedule loop's other steps before create-array has ever run, matching
// GetPool's own best-effort handling of ErrNoArray.
func TestSpaceAlertRunner_NoArrayYetIsNoOp(t *testing.T) {
	arrays := newSpaceAlertTestArrayStore(t)
	notifier := &fakeSpaceAlertNotifier{}
	r := &spaceAlertRunner{Array: arrays, Statter: fakeThresholdStatter{}, Notifier: notifier}

	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(notifier.nearCalls) != 0 || len(notifier.rebalanceCalls) != 0 {
		t.Fatal("expected no publishes with no array topology configured")
	}
}

// TestSpaceAlertRunner_NilArrayStoreIsNoOp proves the check no-ops rather
// than panicking when ArrayStore is unavailable, mirroring
// moverThresholdRunner's own nil-dependency guard.
func TestSpaceAlertRunner_NilArrayStoreIsNoOp(t *testing.T) {
	r := &spaceAlertRunner{Statter: fakeThresholdStatter{}, Notifier: &fakeSpaceAlertNotifier{}}
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
}
