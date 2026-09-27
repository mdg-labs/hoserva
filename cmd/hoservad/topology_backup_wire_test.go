package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// TestWireTopologyBackup_SubmitReachesRunReasonWithPreTopologyReason
// reproduces #406: doc 10 §1 promises a config backup before every
// array-topology change, but before this fix nothing between
// Scheduler.Submit and a disk-topology job's own run ever called one. It
// builds the scheduler through wireTopologyBackup — the exact function
// main.go calls, never a hand copy of the assignment — and submits a real
// job.TypeDiskAdd the same way POST /disks (AddDisk) does, then confirms
// an archive marked ".pre-topology." exists before the job is even done.
func TestWireTopologyBackup_SubmitReachesRunReasonWithPreTopologyReason(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(dir, "hoservad.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer func() { _ = db.Close() }()
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	destDir := filepath.Join(dir, "backups")
	backupService := &backup.Service{
		DB:    db,
		Paths: backup.Paths{DBPath: dbPath},
		Destinations: []backup.Destination{{
			ID:      "boot",
			Path:    destDir,
			Enabled: true,
			Retention: backup.Retention{
				Daily:   backup.DefaultRetentionDaily,
				Weekly:  backup.DefaultRetentionWeekly,
				Monthly: backup.DefaultRetentionMonthly,
			},
		}},
	}

	registry := job.NewRegistry()
	registry.Register(job.TypeDiskAdd, false, func(context.Context, *job.RunContext) error { return nil })
	scheduler := job.NewScheduler(job.NewStore(db), job.NewLogStore(t.TempDir()), job.NewHub(), registry)

	wireTopologyBackup(scheduler, backupService)

	params, err := json.Marshal(job.DiskAddParams{
		Confirmation: "confirm",
		Disk:         disk.AssignedDisk{Device: "/dev/sdz"},
	})
	if err != nil {
		t.Fatalf("marshaling params: %v", err)
	}
	j, err := scheduler.Submit(ctx, job.TypeDiskAdd, nil, params)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := scheduler.Await(ctx, j.ID); err != nil {
		t.Fatalf("Await: %v", err)
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("reading destination: %v", err)
	}
	var found bool
	for _, e := range entries {
		if !e.IsDir() && strings.Contains(e.Name(), ".pre-topology.") {
			found = true
		}
	}
	if !found {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("expected an archive marked pre-topology in %v, found none", names)
	}
}
