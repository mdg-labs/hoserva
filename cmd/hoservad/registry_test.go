package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newRegistryTestScheduler builds a job.Scheduler backed by a real,
// migrated SQLite database — the same construction run's own registration
// block uses (job.NewStore, job.NewLogStore, job.NewHub, job.NewRegistry)
// — so a test here proves a registry.Register call of the exact shape
// main.go uses actually runs its RunFunc through a real Scheduler.Submit,
// not just that RunFix itself works against a fake (already covered by
// internal/job/params_test.go).
func newRegistryTestScheduler(t *testing.T, registry *job.Registry) *job.Scheduler {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-registry-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	return job.NewScheduler(job.NewStore(db), job.NewLogStore(t.TempDir()), job.NewHub(), registry)
}

// TestRegistry_TypeFixRunsRunFix proves job.TypeFix registered the same
// way main.go registers it — registry.Register(job.TypeFix, false,
// job.RunFix(parityEngine)) — actually reaches parity.Engine.Fix through a
// real Scheduler.Submit, rather than failing with
// job.ErrJobTypeNotRegistered the way an unregistered daemon does today
// (#244). TypeSync/TypeScrub, registered the same way in the same block,
// have no equivalent startup-level test in this package.
func TestRegistry_TypeFixRunsRunFix(t *testing.T) {
	ctx := context.Background()
	eng := parity.NewFakeEngine()
	wantErr := errors.New("fix: scripted failure")
	eng.ScriptFix(nil, wantErr)

	registry := job.NewRegistry()
	registry.Register(job.TypeFix, false, job.RunFix(eng))
	s := newRegistryTestScheduler(t, registry)

	params, err := json.Marshal(job.FixParams{Confirm: true})
	if err != nil {
		t.Fatalf("marshaling FixParams: %v", err)
	}
	j, err := s.Submit(ctx, job.TypeFix, nil, params)
	if err != nil {
		t.Fatalf("Submit(TypeFix): %v — TypeFix must be registered like TypeSync/TypeScrub", err)
	}

	finished, err := s.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusFailed {
		t.Fatalf("status = %s, want failed (scripted Fix error)", finished.Status)
	}
	if finished.ErrorMessage != wantErr.Error() {
		t.Fatalf("error = %q, want %q — job.RunFix(eng) must have reached eng.Fix", finished.ErrorMessage, wantErr.Error())
	}
}
