package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// TestNewBackupService_EncryptsNightlyArchiveThroughRealWiring builds the
// onboarding recipient and backupService exactly the way main.go's run()
// does — api.NewBackupRecipientStore, backup.LoadOrGenerateRecipient,
// newBackupService — never a hand copy of the assignment, and proves the
// nightly config-backup Service.Run (this issue's own "Reachable via"
// entry point) really performs the archive encryption step end to end: a
// destination with Encrypt set receives an age-encrypted archive and its
// identity sidecar, decryptable with the backup passphrase alone (Q80).
func TestNewBackupService_EncryptsNightlyArchiveThroughRealWiring(t *testing.T) {
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

	// Exactly main.go's own sequence: settingsStore/settingsService first,
	// then the onboarding recipient through api.NewBackupRecipientStore
	// and backup.LoadOrGenerateRecipient.
	settingsStore := api.NewSettingsStore(db)
	settingsService := api.NewSettingsService(settingsStore, machineKey)
	backupRecipientStore := api.NewBackupRecipientStore(db)
	recipient, err := backup.LoadOrGenerateRecipient(ctx, machineKey, backupRecipientStore, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerateRecipient: %v", err)
	}

	passphrase := "correct horse battery staple"
	if _, err := settingsService.Update(ctx, api.UpdateGeneralSettingsInput{BackupPassphrase: &passphrase}); err != nil {
		t.Fatalf("setting backup passphrase: %v", err)
	}

	cfg := config{stateDir: filepath.Join(dir, "state"), configRoot: filepath.Join(dir, "etc")}
	if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	backupService, err := newBackupService(ctx, cfg, db, machineKey, recipient, settingsService, disk.NewFakeRunner(), api.NewBackupDestinationStore(db), store.NewArrayStore(db))
	if err != nil {
		t.Fatalf("newBackupService: %v", err)
	}
	// Only the encrypting destination is under test: the seeded boot and
	// pool destinations are removed so this run writes to it alone.
	for _, id := range []string{"boot", "pool"} {
		if err := backupService.RemoveDestination(ctx, id); err != nil {
			t.Fatalf("removing seeded destination %q: %v", id, err)
		}
	}
	destDir := filepath.Join(dir, "remote-dest")
	encrypt := true
	if _, err := backupService.AddDestination(ctx, backup.NewDestination{
		Name: "encrypted", Type: backup.TypeLocal, Path: destDir, Encrypt: &encrypt,
	}); err != nil {
		t.Fatalf("AddDestination: %v", err)
	}
	// Pinned rather than the real clock: Service.Run stages its
	// plaintext archive at a fixed, process-global os.TempDir() path
	// keyed only by this timestamp truncated to the minute (archiveName),
	// so an unpinned clock risks colliding with another concurrently
	// running test package that also exercises backup.Service.Run within
	// the same real-world minute (`go test ./...` runs packages in
	// parallel) — not a race this test itself introduces, but one a
	// fixed Now avoids entirely.
	backupService.Now = func() time.Time { return time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC) }

	if err := backupService.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("reading destination: %v", err)
	}
	var archiveAgePath, sidecarPath string
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".identity.age"):
			sidecarPath = filepath.Join(destDir, e.Name())
		case strings.HasSuffix(e.Name(), ".tar.zst.age"):
			archiveAgePath = filepath.Join(destDir, e.Name())
		}
	}
	if archiveAgePath == "" {
		t.Fatalf("no encrypted archive found among %v", entries)
	}
	if sidecarPath == "" {
		t.Fatalf("no identity sidecar found among %v", entries)
	}

	// Decrypt with only the passphrase — Q80's own promise — proving the
	// wiring produced a genuinely restorable pair of files, not just two
	// files with the right names: recover the identity from the sidecar,
	// then use it to decrypt the archive itself.
	sidecar, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	scryptIdentity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		t.Fatalf("creating scrypt identity: %v", err)
	}
	idReader, err := age.Decrypt(bytes.NewReader(sidecar), scryptIdentity)
	if err != nil {
		t.Fatalf("decrypting identity sidecar: %v", err)
	}
	identityRaw, err := io.ReadAll(idReader)
	if err != nil {
		t.Fatalf("reading decrypted identity sidecar: %v", err)
	}
	if string(identityRaw) != recipient.Identity {
		t.Fatal("recovered identity does not match the onboarding recipient's own identity")
	}

	recoveredIdentity, err := age.ParseX25519Identity(string(identityRaw))
	if err != nil {
		t.Fatalf("parsing recovered identity: %v", err)
	}
	archiveFile, err := os.Open(archiveAgePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archiveFile.Close() }()
	archiveReader, err := age.Decrypt(archiveFile, recoveredIdentity)
	if err != nil {
		t.Fatalf("decrypting archive with recovered identity: %v", err)
	}
	if _, err := io.Copy(io.Discard, archiveReader); err != nil {
		t.Fatalf("reading decrypted archive: %v", err)
	}
}

// A config backup built the way main.go builds it skips the pool destination
// while the array record has a migration pending (#639), reading that state
// from the same ArrayStore the daemon holds.
func TestNewBackupService_SkipsThePoolWhileTheArrayRecordHasAMigrationPending(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(dir, "hoservad.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	machineKey, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(dir, "secret.key"), &auth.FakeMachineKeyStore{})
	if err != nil {
		t.Fatal(err)
	}
	settingsService := api.NewSettingsService(api.NewSettingsStore(db), machineKey)
	passphrase := "correct horse battery staple"
	if _, err := settingsService.Update(ctx, api.UpdateGeneralSettingsInput{BackupPassphrase: &passphrase}); err != nil {
		t.Fatal(err)
	}
	recipient, err := backup.LoadOrGenerateRecipient(ctx, machineKey, api.NewBackupRecipientStore(db), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{stateDir: filepath.Join(dir, "state"), configRoot: filepath.Join(dir, "etc")}
	if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	arrays := store.NewArrayStore(db)
	svc, err := newBackupService(ctx, cfg, db, machineKey, recipient, settingsService, disk.NewFakeRunner(), api.NewBackupDestinationStore(db), arrays)
	if err != nil {
		t.Fatal(err)
	}

	poolRoot := filepath.Join(dir, "mnt", "user")
	if err := os.MkdirAll(poolRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.PoolRoot = poolRoot
	svc.PoolMounted = func(string) (bool, error) { return true, nil }
	svc.Log = func(string, ...any) {}
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC) }
	if err := svc.RemoveDestination(ctx, "boot"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveDestination(ctx, "pool"); err != nil {
		t.Fatal(err)
	}
	poolDest := filepath.Join(poolRoot, "hoserva-backups")
	if _, err := svc.AddDestination(ctx, backup.NewDestination{Name: "Pool", Type: backup.TypeLocal, Path: poolDest}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO array_settings (id, create_policy, min_free_space, created_at, migration_pending) VALUES (1, 'mfs', '50G', '2026-10-05T00:00:00Z', 1)`); err != nil {
		t.Fatal(err)
	}

	if err := svc.Run(ctx); err == nil || !strings.Contains(err.Error(), "migration is finished") {
		t.Fatalf("Run = %v, want a failure naming the pending migration", err)
	}
	if _, err := os.Stat(poolDest); !os.IsNotExist(err) {
		t.Fatalf("the pool was written to while the migration was pending: %v", err)
	}

	if _, err := db.ExecContext(ctx, `UPDATE array_settings SET migration_pending = 0`); err != nil {
		t.Fatal(err)
	}
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run after the migration finished: %v", err)
	}
	if entries, err := os.ReadDir(poolDest); err != nil || len(entries) == 0 {
		t.Fatalf("pool destination after the migration finished = %v, %v, want an archive", entries, err)
	}
}
