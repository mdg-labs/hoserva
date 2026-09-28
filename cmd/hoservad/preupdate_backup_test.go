package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// TestNewUpdateEngine_BackupFieldMarksArchivePreUpdate proves the Backup
// field newUpdateEngine itself builds (#401) — the one update.Engine.Reboot
// actually calls — reaches backup.Service.RunReason with
// backup.ReasonPreUpdate, not the unmarked Run a self-update used before.
// It calls newUpdateEngine, never a hand-built adapter, so reverting
// update.go's `Backup: preUpdateBackup{svc: backupSvc}` back to `Backup:
// backupSvc` makes it fail: engine.Backup.Run would then write an unmarked
// archive with no ".pre-update." infix, leaving it exposed to the same
// same-day pruning as an ordinary nightly backup.
func TestNewUpdateEngine_BackupFieldMarksArchivePreUpdate(t *testing.T) {
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

	machineKey, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(dir, "secret.key"), &auth.FakeMachineKeyStore{})
	if err != nil {
		t.Fatalf("LoadOrGenerateMachineKey: %v", err)
	}
	settingsService := api.NewSettingsService(api.NewSettingsStore(db), machineKey)
	notifyService := notify.NewService(notify.NewStore(db), machineKey, nil)

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

	cfg := config{stateDir: filepath.Join(dir, "state")}
	engine := newUpdateEngine(ctx, cfg, db, machineKey, settingsService, nil, func() *job.ArraySequence { return nil }, notifyService, disk.NewFakeRunner(), backupService)

	if err := engine.Backup.Run(ctx); err != nil {
		t.Fatalf("engine.Backup.Run: %v", err)
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("reading destination: %v", err)
	}
	var found bool
	for _, e := range entries {
		if !e.IsDir() && strings.Contains(e.Name(), ".pre-update.") {
			found = true
		}
	}
	if !found {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("expected an archive marked pre-update in %v, found none", names)
	}
}
