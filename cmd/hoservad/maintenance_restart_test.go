package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// maintenanceRestartWiring is one "process" of
// TestArrayStop_SurvivesHoservadRestart below: a real *job.Scheduler and
// *api.Handler backed by a real, migrated SQLite database file, and a
// real *storageTargetSync — exactly the pieces main.go's own run() builds
// between opening the database and serving a request that could submit a
// job or evaluate the storage-target gate. Calling this twice against the
// same dbPath, closing the first db in between, is what "a hoservad
// restart" means for this test: nothing survives except what the first
// process actually persisted to SQLite.
//
// The array sequence itself is a hand-built fake (fakeArraySequence,
// below), never job.ArraySequence built through this package's own
// newArraySequence: that always wires the catch-all through
// pool.SystemdMounter against the real, production /mnt/user path
// (CLAUDE.md's own "real disks are off-limits" — a pre-fix Scheduler that
// wrongly left maintenance mode would otherwise have this test's own
// Startup call actually try to mkdir it). Nothing in this file ever calls
// a Runner that is not disk.FakeRunner.
type maintenanceRestartWiring struct {
	db            *sql.DB
	scheduler     *job.Scheduler
	registry      *job.Registry
	handler       *api.Handler
	runner        *disk.FakeRunner
	storageTarget *storageTargetSync
}

func newMaintenanceRestartWiring(t *testing.T, dbPath string) *maintenanceRestartWiring {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	runnerApply := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runnerApply.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	registry := job.NewRegistry()
	scheduler := job.NewScheduler(job.NewStore(db), job.NewLogStore(t.TempDir()), job.NewHub(), registry)

	// Exactly what main.go's own run() does at startup, in this order:
	// recover interrupted jobs, then restore whatever maintenance state
	// `array stop` last persisted (#387) — before anything below can admit
	// a job or evaluate the storage-target gate.
	if err := scheduler.RecoverFromRestart(context.Background()); err != nil {
		t.Fatalf("RecoverFromRestart: %v", err)
	}
	if err := scheduler.RestorePersistedMaintenance(context.Background()); err != nil {
		t.Fatalf("RestorePersistedMaintenance: %v", err)
	}

	fakeRunner := disk.NewFakeRunner()
	storageTarget := &storageTargetSync{
		Generator: cfggen.NewGenerator(t.TempDir()),
		Runner:    fakeRunner,
		FlagPath:  filepath.Join(t.TempDir(), "storage-ready"),
		// Every mount in this test is fakeArraySequence's own
		// storageTargetTestMount, never a real one — PoolMounted stands in
		// for the real pool.IsMountedConfirmed stat call the same way
		// cmd/hoservad's other storageTargetSync tests already do.
		PoolMounted: func(string) (bool, error) { return true, nil },
	}

	return &maintenanceRestartWiring{
		db:            db,
		scheduler:     scheduler,
		registry:      registry,
		handler:       &api.Handler{Scheduler: scheduler, Store: job.NewStore(db)},
		runner:        fakeRunner,
		storageTarget: storageTarget,
	}
}

// fakeArraySequence builds a job.ArraySequence around w's own real
// Scheduler and storageTargetSync, with every mount/service/gate a safe,
// in-memory fake: storageTargetTestMount and storageTargetTestGate are
// this package's own fakes (storagetarget_test.go), reused here so this
// file never needs its own. mountCalls is storageTargetTestMount's own
// call counter — a test asserts on it directly, rather than scanning
// w.runner's calls for a real mount unit's systemctl argv, so this stays
// correct even if the production mount path's own argv ever changes.
func fakeArraySequence(w *maintenanceRestartWiring, mountCalls *int32) *job.ArraySequence {
	return &job.ArraySequence{
		Scheduler:     w.scheduler,
		Gate:          storageTargetTestGate{ready: true},
		CatchAll:      storageTargetTestMount{where: pool.CatchAllPath, calls: mountCalls},
		StorageTarget: w.storageTarget,
	}
}

// TestArrayStop_SurvivesHoservadRestart is #387's own central data-safety
// regression: before this fix, job.Scheduler's maintenance state lived
// only in memory, so a hoservad restart while a user had `array stop`
// active — a crash, a package-upgrade restart, an update reboot of the
// daemon alone — silently returned to normal operation. This proves the
// fix across a simulated restart against the very database the first
// process wrote: the second process's own Scheduler refuses a job with
// ErrMaintenanceMode without ever calling EnterMaintenance itself, and its
// own storageTargetSync.Startup — run against a gate reporting ready —
// mounts nothing and leaves the readiness flag cleared, exactly as doc 02
// §1/§4 (Q70) requires.
func TestArrayStop_SurvivesHoservadRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "array-stop-restart.db")

	// Process 1: stop the array through the real `POST /array/stop`
	// handler.
	w1 := newMaintenanceRestartWiring(t, dbPath)
	var mountCalls1 int32
	seq1 := fakeArraySequence(w1, &mountCalls1)
	w1.handler.SetArray(seq1)

	if _, err := w1.handler.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	if !w1.scheduler.InMaintenance() {
		t.Fatal("InMaintenance() = false immediately after StopArray")
	}
	if _, err := os.Stat(w1.storageTarget.flagPath()); err == nil {
		t.Fatal("the readiness flag still exists after StopArray — the storage-target gate did not close")
	}

	// "Restart": the first process's own database connection closes, a
	// second process opens the same file and runs exactly the startup
	// sequence main.go's own run() does, before serving anything.
	if err := w1.db.Close(); err != nil {
		t.Fatalf("closing the first process's database: %v", err)
	}

	w2 := newMaintenanceRestartWiring(t, dbPath)
	t.Cleanup(func() { _ = w2.db.Close() })

	if !w2.scheduler.InMaintenance() {
		t.Fatal("a freshly restored Scheduler reports InMaintenance() = false — `array stop` did not survive the restart (#387)")
	}

	w2.registry.Register(job.TypeSync, false, func(ctx context.Context, rc *job.RunContext) error { return nil })
	if _, err := w2.scheduler.Submit(ctx, job.TypeSync, nil, nil); !errors.Is(err, job.ErrMaintenanceMode) {
		t.Fatalf("Submit after restart = %v, want ErrMaintenanceMode", err)
	}

	// The restored gate still reports every disk present (storageTargetTestGate
	// always ready:true) — proving what withholds the mount below is the
	// restored maintenance state, never a degraded gate.
	var mountCalls2 int32
	seq2 := fakeArraySequence(w2, &mountCalls2)
	if err := w2.storageTarget.Startup(ctx, seq2); err != nil {
		t.Fatalf("restarted Startup: %v", err)
	}
	if w2.storageTarget.Ready() {
		t.Fatal("storageTargetSync.Ready() = true after a restart with persisted maintenance mode — the gate opened over a disk swap the user believed was still stopped")
	}
	if atomic.LoadInt32(&mountCalls2) != 0 {
		t.Fatalf("restarted Startup mounted the pool (%d call(s)) — maintenance mode did not survive the restart", mountCalls2)
	}
	if _, err := os.Stat(w2.storageTarget.flagPath()); err == nil {
		t.Fatal("the readiness flag exists after the restarted Startup — a dependent service could still pass hoserva-storage.target")
	}
}

// TestArrayStop_RebootDoesNotPersistMaintenance_RestartRestoresNormalOperation
// is #387 finding 2's own regression for the Reboot path:
// updateShutdownLookup.Stop (cmd/hoservad/update.go) calls
// job.ArraySequence.StopForShutdown, never Stop, exactly the way this
// test drives it directly — a Reboot is not a user asking the array to
// stay stopped once the box comes back up, and persisting a new "stopped"
// state here would leave RestorePersistedMaintenance holding the array
// offline after the next ordinary boot, with every scheduled sync and
// mover run refused until someone runs `array start` against an array
// that was never actually asked to stay down.
func TestArrayStop_RebootDoesNotPersistMaintenance_RestartRestoresNormalOperation(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "array-reboot-restart.db")

	w1 := newMaintenanceRestartWiring(t, dbPath)
	var mountCalls1 int32
	seq1 := fakeArraySequence(w1, &mountCalls1)

	if err := seq1.StopForShutdown(ctx); err != nil {
		t.Fatalf("StopForShutdown (simulated Reboot): %v", err)
	}
	if !w1.scheduler.InMaintenance() {
		t.Fatal("InMaintenance() = false immediately after StopForShutdown — the stop sequence itself must still take effect for this process's own lifetime")
	}

	if err := w1.db.Close(); err != nil {
		t.Fatalf("closing the first process's database: %v", err)
	}

	w2 := newMaintenanceRestartWiring(t, dbPath)
	t.Cleanup(func() { _ = w2.db.Close() })

	if w2.scheduler.InMaintenance() {
		t.Fatal("a freshly restored Scheduler reports InMaintenance() = true after only a simulated Reboot (#387 finding 2) — a plain reboot must not leave the array stuck in maintenance mode after the next boot")
	}
}

// TestArrayStop_UserStopThenReboot_StaysStoppedAfterRestart proves the
// other half of the same criterion: a persisted user `array stop` already
// in force must survive a later Reboot, not be cleared or left ambiguous
// by it.
func TestArrayStop_UserStopThenReboot_StaysStoppedAfterRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "array-userstop-reboot-restart.db")

	w1 := newMaintenanceRestartWiring(t, dbPath)
	var mountCalls1 int32
	seq1 := fakeArraySequence(w1, &mountCalls1)
	w1.handler.SetArray(seq1)

	if _, err := w1.handler.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	if err := seq1.StopForShutdown(ctx); err != nil {
		t.Fatalf("StopForShutdown (simulated Reboot right after a user array stop): %v", err)
	}

	if err := w1.db.Close(); err != nil {
		t.Fatalf("closing the first process's database: %v", err)
	}

	w2 := newMaintenanceRestartWiring(t, dbPath)
	t.Cleanup(func() { _ = w2.db.Close() })

	if !w2.scheduler.InMaintenance() {
		t.Fatal("InMaintenance() = false on a restarted Scheduler after a user `array stop` was already in force before a simulated Reboot (#387 finding 2) — a persisted user stop must survive a later shutdown-sequence caller")
	}
}
