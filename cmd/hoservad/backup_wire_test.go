package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// TestWireBackup_HandlerServesExportConfigInsteadOf501 reproduces #269's
// first finding: before this fix, main.go's handler.* assignment block
// never set Backup, so ExportConfig/ImportConfig (POST /config/export,
// POST /config/import) 501'd unconditionally regardless of array or
// onboarding state. This builds the handler through wireBackup — the
// exact function main.go calls, never a hand copy of the assignment —
// and confirms a real ExportConfig call reaches backup.Service instead
// of the 501 a nil Handler.Backup would still produce.
func TestWireBackup_HandlerServesExportConfigInsteadOf501(t *testing.T) {
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

	backupService := &backup.Service{
		DB:    db,
		Paths: backup.Paths{DBPath: dbPath},
	}

	h := &api.Handler{}
	wireBackup(h, backupService)

	got, err := h.ExportConfig(ctx)
	if err != nil {
		t.Fatalf("ExportConfig: %v", err)
	}
	data, err := io.ReadAll(got.Data)
	if err != nil {
		t.Fatalf("reading exported archive: %v", err)
	}
	if c, ok := got.Data.(io.Closer); ok {
		_ = c.Close()
	}
	if len(data) == 0 {
		t.Fatal("ExportConfig returned an empty archive")
	}
}

// TestNewBackupService_NoArchiveHoldsTheMachineKey builds the backup service
// through newBackupService, as run() does, with the machine key where
// --machine-key-path puts it inside the config root, and takes every archive
// the daemon takes: the nightly run, the pre-import, pre-update and
// pre-topology archives, and the config export. None may carry a copy of
// the key (#473).
func TestNewBackupService_NoArchiveHoldsTheMachineKey(t *testing.T) {
	for name, keyRel := range map[string]string{
		"packaged name":           "secret.key",
		"another name and folder": filepath.Join("keys", "host-seal.bin"),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()

			migrations, err := store.Load()
			if err != nil {
				t.Fatalf("loading embedded migrations: %v", err)
			}
			db, err := sql.Open("sqlite", store.DSN(filepath.Join(dir, "hoservad.db")))
			if err != nil {
				t.Fatalf("opening test database: %v", err)
			}
			defer func() { _ = db.Close() }()
			runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
			if _, _, err := runner.Apply(ctx); err != nil {
				t.Fatalf("applying migrations: %v", err)
			}

			cfg := config{
				stateDir:       filepath.Join(dir, "state"),
				configRoot:     filepath.Join(dir, "etc"),
				machineKeyPath: filepath.Join(dir, "etc", "hoserva", keyRel),
			}
			if err := os.MkdirAll(filepath.Join(cfg.configRoot, "hoserva"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.configRoot, "hoserva", "smb.custom.conf"), []byte("guest ok = no\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(cfg.machineKeyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			machineKey, err := auth.LoadOrGenerateMachineKey(ctx, cfg.machineKeyPath, &auth.FakeMachineKeyStore{})
			if err != nil {
				t.Fatalf("LoadOrGenerateMachineKey: %v", err)
			}
			keyBytes, err := os.ReadFile(cfg.machineKeyPath)
			if err != nil || len(keyBytes) == 0 {
				t.Fatalf("reading the generated machine key: %v", err)
			}

			settings := api.NewSettingsService(api.NewSettingsStore(db), machineKey)
			svc, err := newBackupService(ctx, cfg, db, machineKey, nil, settings, disk.NewFakeRunner(), api.NewBackupDestinationStore(db))
			if err != nil {
				t.Fatalf("newBackupService: %v", err)
			}
			if err := svc.RemoveDestination(ctx, "pool"); err != nil {
				t.Fatalf("removing the seeded pool destination: %v", err)
			}

			requireNoKey := func(label, archive string) {
				t.Helper()
				tree, err := backup.ExtractVerifiedArchive(archive)
				if err != nil {
					t.Fatalf("%s: extracting: %v", label, err)
				}
				defer func() { _ = os.RemoveAll(tree) }()
				var files int
				err = filepath.WalkDir(tree, func(path string, d os.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						return err
					}
					files++
					body, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					if bytes.Contains(body, keyBytes) {
						rel, _ := filepath.Rel(tree, path)
						t.Errorf("%s: the archive holds a copy of the machine key at %s", label, rel)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(tree, "custom", "smb.custom.conf")); err != nil {
					t.Errorf("%s: the archive lacks the config root's custom file, so it was not built from it: %v", label, err)
				}
			}

			for _, reason := range []backup.Reason{backup.ReasonNone, backup.ReasonPreImport, backup.ReasonPreUpdate, backup.ReasonPreTopology} {
				written, err := svc.RunReasonArchive(ctx, reason)
				if err != nil {
					t.Fatalf("RunReasonArchive(%q): %v", reason, err)
				}
				requireNoKey("reason "+string(reason), filepath.Join(cfg.stateDir, "backups", written.Name))
			}

			h := &api.Handler{}
			wireBackup(h, svc)
			got, err := h.ExportConfig(ctx)
			if err != nil {
				t.Fatalf("ExportConfig: %v", err)
			}
			exported := filepath.Join(dir, "export.tar.zst")
			out, err := os.Create(exported)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(out, got.Data); err != nil {
				t.Fatal(err)
			}
			_ = out.Close()
			if c, ok := got.Data.(io.Closer); ok {
				_ = c.Close()
			}
			requireNoKey("export", exported)
		})
	}
}
