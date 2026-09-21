package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// fakeThresholdStatter is a scriptable pool.SpaceStatter, mirroring
// internal/pool's own unexported fakeSpaceStatter (space_test.go) — this
// package needs its own since that one is not exported.
type fakeThresholdStatter struct {
	stats map[string]pool.SpaceStat
}

func (f fakeThresholdStatter) StatSpace(_ context.Context, path string) (pool.SpaceStat, error) {
	return f.stats[path], nil
}

type moverThresholdHarness struct {
	arrays    *store.ArrayStore
	jobs      *job.Store
	scheduler *job.Scheduler
	registry  *job.Registry
}

func newMoverThresholdHarness(t *testing.T) *moverThresholdHarness {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-mover-threshold-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	jobStore := job.NewStore(db)
	registry := job.NewRegistry()
	scheduler := job.NewScheduler(jobStore, job.NewLogStore(t.TempDir()), job.NewHub(), registry)
	return &moverThresholdHarness{
		arrays:    store.NewArrayStore(db),
		jobs:      jobStore,
		scheduler: scheduler,
		registry:  registry,
	}
}

func putThresholdTestArray(t *testing.T, arrays *store.ArrayStore) {
	t.Helper()
	if err := arrays.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "20G",
		CreatedAt:    time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Serial: "DATA1", ByIDName: "wwn-wwn-d1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdd", Filesystem: "xfs", FSUUID: "uuid-c1", WWN: "wwn-c1", Serial: "CACHE1", ByIDName: "wwn-wwn-c1", Mountpoint: "/mnt/cache"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
}

// TestMoverThresholdRunner_SubmitsMoverAboveThreshold proves a cache disk
// at or above cache.DefaultThresholdPercent (doc 09 §2's threshold
// trigger) submits a TypeMover job through the exact registration #53
// added.
func TestMoverThresholdRunner_SubmitsMoverAboveThreshold(t *testing.T) {
	h := newMoverThresholdHarness(t)
	putThresholdTestArray(t, h.arrays)
	ran := make(chan struct{})
	h.registry.Register(job.TypeMover, true, func(context.Context, *job.RunContext) error {
		close(ran)
		return nil
	})

	r := &moverThresholdRunner{
		Scheduler: h.scheduler,
		Jobs:      h.jobs,
		Array:     h.arrays,
		Statter: fakeThresholdStatter{stats: map[string]pool.SpaceStat{
			"/mnt/cache": {TotalBytes: 100, FreeBytes: 10}, // 90% used
		}},
	}
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a TypeMover job to run once the cache disk crossed the threshold")
	}
	awaitNoActiveMoverJobs(t, h.jobs)
}

// TestMoverThresholdRunner_BelowThresholdSubmitsNothing proves a cache
// disk below the threshold never submits a job — doc 09 §2's "not
// continuous" trigger only fires once the cache is actually full enough.
func TestMoverThresholdRunner_BelowThresholdSubmitsNothing(t *testing.T) {
	h := newMoverThresholdHarness(t)
	putThresholdTestArray(t, h.arrays)
	var ran bool
	h.registry.Register(job.TypeMover, true, func(context.Context, *job.RunContext) error {
		ran = true
		return nil
	})

	r := &moverThresholdRunner{
		Scheduler: h.scheduler,
		Jobs:      h.jobs,
		Array:     h.arrays,
		Statter: fakeThresholdStatter{stats: map[string]pool.SpaceStat{
			"/mnt/cache": {TotalBytes: 100, FreeBytes: 50}, // 50% used
		}},
	}
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if ran {
		t.Fatal("mover ran below the threshold")
	}
}

// TestMoverThresholdRunner_SkipsSubmitWhenMoverAlreadyActive proves a
// second tick while a mover run is still queued or running does not queue
// a redundant one behind it.
func TestMoverThresholdRunner_SkipsSubmitWhenMoverAlreadyActive(t *testing.T) {
	h := newMoverThresholdHarness(t)
	putThresholdTestArray(t, h.arrays)
	started := make(chan struct{})
	release := make(chan struct{})
	h.registry.Register(job.TypeMover, true, func(ctx context.Context, rc *job.RunContext) error {
		close(started)
		<-release
		return nil
	})

	r := &moverThresholdRunner{
		Scheduler: h.scheduler,
		Jobs:      h.jobs,
		Array:     h.arrays,
		Statter: fakeThresholdStatter{stats: map[string]pool.SpaceStat{
			"/mnt/cache": {TotalBytes: 100, FreeBytes: 10},
		}},
	}
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	<-started

	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}

	active, err := h.jobs.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	var moverJobs int
	for _, j := range active {
		if j.Type == job.TypeMover {
			moverJobs++
		}
	}
	if moverJobs > 1 {
		t.Fatalf("moverJobs = %d, want at most 1 — a second tick queued a redundant run", moverJobs)
	}

	close(release)
	awaitNoActiveMoverJobs(t, h.jobs)
}

// awaitNoActiveMoverJobs waits for every TypeMover job to leave the active
// set — used only to let a background job finish recording its terminal
// status before the test's own database is closed by t.Cleanup.
func awaitNoActiveMoverJobs(t *testing.T, jobs *job.Store) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		active, err := jobs.ListActive(context.Background())
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		none := true
		for _, j := range active {
			if j.Type == job.TypeMover {
				none = false
				break
			}
		}
		if none {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for mover jobs to finish")
}

// TestMoverThresholdRunner_NoArrayYetSubmitsNothing proves the poll treats
// store.ErrNoArray the same "nothing to do yet" way moverSharesFromStore
// does — the daemon can run this loop before create-array has ever run.
func TestMoverThresholdRunner_NoArrayYetSubmitsNothing(t *testing.T) {
	h := newMoverThresholdHarness(t)
	var ran bool
	h.registry.Register(job.TypeMover, true, func(context.Context, *job.RunContext) error {
		ran = true
		return nil
	})

	r := &moverThresholdRunner{
		Scheduler: h.scheduler,
		Jobs:      h.jobs,
		Array:     h.arrays,
		Statter:   fakeThresholdStatter{},
	}
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if ran {
		t.Fatal("mover ran with no array configured")
	}
}
