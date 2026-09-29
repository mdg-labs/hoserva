package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

type backupRig struct {
	svc      *backup.Service
	db       *sql.DB
	cfg      config
	settings *api.SettingsService
	// rebuild runs newBackupService again against the same database, as a
	// daemon restart does.
	rebuild func() (*backup.Service, error)
}

// newBackupRig builds the backup service the way main.go's run() does:
// a migrated database, the machine key, the settings service, the
// onboarding recipient, and newBackupService with a
// api.BackupDestinationStore.
func newBackupRig(t *testing.T, passphrase string) *backupRig {
	t.Helper()
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
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	machineKey, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(dir, "secret.key"), &auth.FakeMachineKeyStore{})
	if err != nil {
		t.Fatalf("LoadOrGenerateMachineKey: %v", err)
	}
	settings := api.NewSettingsService(api.NewSettingsStore(db), machineKey)
	if passphrase != "" {
		if _, err := settings.Update(ctx, api.UpdateGeneralSettingsInput{BackupPassphrase: &passphrase}); err != nil {
			t.Fatalf("setting backup passphrase: %v", err)
		}
	}
	recipient, err := backup.LoadOrGenerateRecipient(ctx, machineKey, api.NewBackupRecipientStore(db), nil)
	if err != nil {
		t.Fatalf("LoadOrGenerateRecipient: %v", err)
	}
	cfg := config{stateDir: filepath.Join(dir, "state"), configRoot: filepath.Join(dir, "etc")}
	if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	build := func() (*backup.Service, error) {
		return newBackupService(ctx, cfg, db, machineKey, recipient, settings, disk.NewFakeRunner(), api.NewBackupDestinationStore(db))
	}
	svc, err := build()
	if err != nil {
		t.Fatalf("newBackupService: %v", err)
	}
	// Service.Run stages at a process-global temp path keyed by this
	// timestamp, so a fixed clock keeps concurrent test packages apart.
	svc.Now = func() time.Time { return time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC) }
	return &backupRig{svc: svc, db: db, cfg: cfg, settings: settings, rebuild: build}
}

func TestNewBackupService_ConfiguresBootAndPoolDestinations(t *testing.T) {
	ctx := context.Background()
	rig := newBackupRig(t, "")

	dests, err := rig.svc.ListDestinations(ctx)
	if err != nil {
		t.Fatalf("ListDestinations: %v", err)
	}
	if len(dests) != 2 {
		t.Fatalf("destinations = %+v, want the boot and pool defaults", dests)
	}
	boot, pool := dests[0], dests[1]
	if boot.ID != "boot" || !boot.Enabled || boot.Path != filepath.Join(rig.cfg.stateDir, "backups") {
		t.Fatalf("boot = %+v", boot)
	}
	if pool.ID != "pool" || !pool.Enabled || pool.Path != backup.DefaultPoolDestination {
		t.Fatalf("pool = %+v", pool)
	}

	// The pool is not mounted on this host: Q40's second destination is
	// skipped (#409) and the boot one is still written.
	rig.svc.PoolMounted = func(string) (bool, error) { return false, nil }
	rig.svc.Log = func(string, ...any) {}
	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run with the pool unmounted: %v", err)
	}
	entries, err := os.ReadDir(boot.Path)
	if err != nil || len(entries) != 1 {
		t.Fatalf("boot destination holds %v (%v), want the one archive", entries, err)
	}
}

func TestNewBackupService_WritesBothDefaultsWhenThePoolIsMounted(t *testing.T) {
	ctx := context.Background()
	rig := newBackupRig(t, "")

	// The seeded pool path is under /mnt/user, which a unit test must
	// never write: the same destination is re-pointed at a directory this
	// test owns, under a PoolRoot it reports mounted.
	poolRoot := filepath.Join(t.TempDir(), "mnt", "user")
	rig.svc.PoolRoot = poolRoot
	rig.svc.PoolMounted = func(string) (bool, error) { return true, nil }
	if err := rig.svc.RemoveDestination(ctx, "pool"); err != nil {
		t.Fatal(err)
	}
	poolDir := filepath.Join(poolRoot, "hoserva-backups")
	if _, err := rig.svc.AddDestination(ctx, backup.NewDestination{Name: "Pool", Type: backup.TypeLocal, Path: poolDir}); err != nil {
		t.Fatal(err)
	}

	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, dir := range []string{filepath.Join(rig.cfg.stateDir, "backups"), poolDir} {
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
			t.Fatalf("%s holds %v (%v), want the one archive", dir, entries, err)
		}
	}
}

func TestWireBackup_HandlerServesDestinationOperationsAndEncryptsRemoteWrites(t *testing.T) {
	ctx := context.Background()
	rig := newBackupRig(t, "correct horse")
	rclone := &backup.FakeRclone{}
	rig.svc.Rclone = rclone
	h := &api.Handler{}
	wireBackup(h, rig.svc)

	list, err := h.ListBackupDestinations(ctx)
	if err != nil {
		t.Fatalf("ListBackupDestinations: %v — the wired handler must not 501", err)
	}
	if len(list.Destinations) != 2 {
		t.Fatalf("listed %d destinations, want the two defaults", len(list.Destinations))
	}

	created, err := h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{
		Name: "Bucket", Type: apiv1.BackupDestinationTypeS3, Path: "bucket/hoserva",
		Options: apiv1.OptCreateBackupDestinationRequestOptions{Set: true, Value: apiv1.CreateBackupDestinationRequestOptions{"access_key_id": "AKIA"}},
		Secrets: apiv1.OptCreateBackupDestinationRequestSecrets{Set: true, Value: apiv1.CreateBackupDestinationRequestSecrets{"secret_access_key": "s3cr3t"}},
	})
	if err != nil {
		t.Fatalf("CreateBackupDestination: %v", err)
	}
	if !created.Encrypt {
		t.Fatal("a remote destination was created without encryption")
	}

	// Only the remote destination is under test here: the boot and pool
	// defaults are removed so the run writes to it alone.
	for _, id := range []string{"boot", "pool"} {
		if err := h.DeleteBackupDestination(ctx, apiv1.DeleteBackupDestinationParams{DestinationId: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawArchive, sawSidecar bool
	for key := range rclone.Files() {
		switch {
		case strings.HasSuffix(key, ".tar.zst"):
			t.Fatalf("a plaintext archive reached the remote: %s", key)
		case strings.HasSuffix(key, ".tar.zst.age"):
			sawArchive = true
		case strings.HasSuffix(key, ".tar.zst.age.identity.age"):
			sawSidecar = true
		}
	}
	if !sawArchive || !sawSidecar {
		t.Fatalf("remote holds %v, want an encrypted archive and its identity sidecar", rclone.Files())
	}
}

func TestWireBackup_RemoteDestinationNeedsAPassphrase(t *testing.T) {
	rig := newBackupRig(t, "")
	rig.svc.Rclone = &backup.FakeRclone{}
	h := &api.Handler{}
	wireBackup(h, rig.svc)

	_, err := h.CreateBackupDestination(context.Background(), &apiv1.CreateBackupDestinationRequest{
		Name: "Bucket", Type: apiv1.BackupDestinationTypeS3, Path: "bucket",
		Options: apiv1.OptCreateBackupDestinationRequestOptions{Set: true, Value: apiv1.CreateBackupDestinationRequestOptions{"access_key_id": "AKIA"}},
		Secrets: apiv1.OptCreateBackupDestinationRequestSecrets{Set: true, Value: apiv1.CreateBackupDestinationRequestSecrets{"secret_access_key": "x"}},
	})
	if ae := h.NewError(context.Background(), err); err == nil || ae.StatusCode != 400 {
		t.Fatalf("err = %v, want a 400", err)
	}
}

type fakeStalePublisher struct {
	mu     sync.Mutex
	events []notify.EventType
	titles []string
}

func (f *fakeStalePublisher) Publish(_ context.Context, event notify.EventType, title, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	f.titles = append(f.titles, title)
	return nil
}

func TestScheduleRunnerTick_AlertsOnceOnAStaleBackupDestination(t *testing.T) {
	ctx := context.Background()
	rig := newBackupRig(t, "")
	created := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	rig.svc.Now = func() time.Time { return created }
	if _, err := rig.svc.AddDestination(ctx, backup.NewDestination{Name: "Old disk", Type: backup.TypeLocal, Path: filepath.Join(t.TempDir(), "d")}); err != nil {
		t.Fatal(err)
	}

	pub := &fakeStalePublisher{}
	now := created.Add(10 * 24 * time.Hour)
	r := &scheduleRunner{BackupStale: &staleDestinationChecker{svc: rig.svc, publisher: pub, now: func() time.Time { return now }}}

	if err := r.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if err := r.tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	// The seeded boot and pool destinations were created at the real
	// clock's now, a day before this tick's, so only the disk added ten
	// days back is stale — and it alerts once over the two ticks.
	if len(pub.events) != 1 || !strings.Contains(pub.titles[0], "Old disk") {
		t.Fatalf("published %d alerts over two ticks (%v), want one, for Old disk", len(pub.events), pub.titles)
	}
	for _, e := range pub.events {
		if e != notify.EventBackupDestinationStale {
			t.Fatalf("event = %q, want %q", e, notify.EventBackupDestinationStale)
		}
	}
}
