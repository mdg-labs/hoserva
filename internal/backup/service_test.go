package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE secrets (id INTEGER PRIMARY KEY, token BLOB); INSERT INTO secrets VALUES (1, x'5a5a');"); err != nil {
		t.Fatalf("seeding database: %v", err)
	}
	return db
}

func testLayout(t *testing.T) (Paths, string) {
	root := t.TempDir()
	configRoot := filepath.Join(root, "etc")
	stacks := filepath.Join(root, "stacks")
	templates := filepath.Join(root, "templates")
	stateDir := filepath.Join(root, "var")

	if err := os.MkdirAll(filepath.Join(configRoot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "snapraid.conf"), []byte("content /mnt/disk1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "smb.custom.conf"), []byte("custom"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stacks, "plex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stacks, "plex", "docker-compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stacks, "plex", ".env"), []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(templates, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templates, "plex.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "jobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "metrics.db"), []byte("metrics"), 0o600); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(stateDir, "hoserva.db")
	return Paths{
		StateDir:     stateDir,
		ConfigRoot:   configRoot,
		DBPath:       dbPath,
		StacksDir:    stacks,
		TemplatesDir: templates,
	}, root
}

func TestService_RunCreatesVerifiedArchive(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)
	destDir := filepath.Join(root, "backups")

	now := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	svc := &Service{
		DB:    db,
		Paths: paths,
		Secrets: &FakeSecretSource{
			Passphrase: "backup-pass",
			HasPass:    true,
			Secrets: []DatabaseSecret{{
				Table:      "secrets",
				Column:     "token",
				RowID:      "1",
				Ciphertext: []byte{0x00},
			}},
		},
		Cipher: FakeSecretCipher{},
		Destinations: []Destination{{
			ID:      "local",
			Path:    destDir,
			Enabled: true,
			Retention: Retention{
				Daily:   7,
				Weekly:  4,
				Monthly: 6,
			},
		}},
		Hostname: "test-host",
		Version:  "0.0.0-test",
		Now:      func() time.Time { return now },
	}

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	name := archiveName(svc.installationID(), now, ReasonNone, 0)
	archivePath := filepath.Join(destDir, name)
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	if err := VerifyArchive(archivePath, "backup-pass"); err != nil {
		t.Fatalf("VerifyArchive: %v", err)
	}

	verifyDir := t.TempDir()
	if err := unpackArchive(archivePath, verifyDir); err != nil {
		t.Fatalf("unpackArchive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, "state.db")); err != nil {
		t.Fatalf("state.db missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, "generated", "snapraid.conf")); err != nil {
		t.Fatalf("generated config missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, "custom", "smb.custom.conf")); err != nil {
		t.Fatalf("custom config missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, "stacks", "plex", "docker-compose.yml")); err != nil {
		t.Fatalf("stack compose missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, "stacks", "plex", ".env")); err == nil {
		t.Fatal("stack .env must not appear in archive in plain text")
	}
	if _, err := os.Stat(filepath.Join(verifyDir, "secrets.age")); err != nil {
		t.Fatalf("secrets.age missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, "metrics.db")); err == nil {
		t.Fatal("metrics.db must be excluded")
	}
}

// TestService_RunSkipsUnmountedPoolDestinationAndWritesBoot reproduces
// #409's data-loss scenario: with the array stopped, CatchAllPath is a
// bare, empty directory on the root filesystem — before this fix,
// writeArchive had no mount check at all, so RunReason would create the
// pool destination's directory right there and write the archive into it,
// reporting success, and that archive would be hidden the moment the pool
// mounted back over it. With PoolMounted reporting the pool unmounted, the
// pool destination must receive nothing at all, while the boot destination
// — unaffected by the array's own mount state — is written normally.
func TestService_RunSkipsUnmountedPoolDestinationAndWritesBoot(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)

	poolRoot := filepath.Join(root, "mnt", "user")
	poolDest := filepath.Join(poolRoot, "hoserva-backups")
	bootDest := filepath.Join(root, "boot-backups")

	now := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	svc := &Service{
		DB:    db,
		Paths: paths,
		Secrets: &FakeSecretSource{
			Passphrase: "backup-pass",
			HasPass:    true,
		},
		Cipher: FakeSecretCipher{},
		Destinations: []Destination{
			{ID: "boot", Path: bootDest, Enabled: true, Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}},
			{ID: "pool", Path: poolDest, Enabled: true, Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}},
		},
		Hostname:    "test-host",
		Version:     "0.0.0-test",
		Now:         func() time.Time { return now },
		PoolRoot:    poolRoot,
		PoolMounted: func(string) (bool, error) { return false, nil },
	}

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	name := archiveName(svc.installationID(), now, ReasonNone, 0)
	if _, err := os.Stat(filepath.Join(bootDest, name)); err != nil {
		t.Fatalf("boot archive missing: %v", err)
	}
	if _, err := os.Stat(poolDest); !os.IsNotExist(err) {
		t.Fatalf("pool destination %q must not exist — the pool was reported unmounted (#409): stat error = %v", poolDest, err)
	}
	if _, err := os.Stat(poolRoot); !os.IsNotExist(err) {
		t.Fatalf("pool root %q must not have been created on the root filesystem while unmounted (#409): stat error = %v", poolRoot, err)
	}
}

// TestService_RunWritesPoolDestinationWhenMounted proves the mount check
// added for #409 does not disturb the ordinary case: with PoolMounted
// reporting the pool mounted, a destination under it is written exactly as
// before.
func TestService_RunWritesPoolDestinationWhenMounted(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)

	poolRoot := filepath.Join(root, "mnt", "user")
	if err := os.MkdirAll(poolRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	poolDest := filepath.Join(poolRoot, "hoserva-backups")

	now := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	svc := &Service{
		DB:    db,
		Paths: paths,
		Secrets: &FakeSecretSource{
			Passphrase: "backup-pass",
			HasPass:    true,
		},
		Cipher: FakeSecretCipher{},
		Destinations: []Destination{
			{ID: "pool", Path: poolDest, Enabled: true, Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}},
		},
		Hostname:    "test-host",
		Version:     "0.0.0-test",
		Now:         func() time.Time { return now },
		PoolRoot:    poolRoot,
		PoolMounted: func(string) (bool, error) { return true, nil },
	}

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	name := archiveName(svc.installationID(), now, ReasonNone, 0)
	if _, err := os.Stat(filepath.Join(poolDest, name)); err != nil {
		t.Fatalf("pool archive missing despite the pool being reported mounted: %v", err)
	}
}

// TestService_RunReasonFailsWhenEveryDestinationIsUnavailable is #409's
// second acceptance criterion: with its only destination under an
// unmounted pool, RunReason must fail rather than report success over an
// archive written nowhere — a pre-topology backup with only this
// destination configured must refuse the topology job it guards rather
// than let it proceed with no snapshot actually taken.
func TestService_RunReasonFailsWhenEveryDestinationIsUnavailable(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)

	poolRoot := filepath.Join(root, "mnt", "user")
	poolDest := filepath.Join(poolRoot, "hoserva-backups")

	svc := &Service{
		DB:    db,
		Paths: paths,
		Secrets: &FakeSecretSource{
			Passphrase: "backup-pass",
			HasPass:    true,
		},
		Cipher: FakeSecretCipher{},
		Destinations: []Destination{
			{ID: "pool", Path: poolDest, Enabled: true, Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}},
		},
		Hostname:    "test-host",
		Version:     "0.0.0-test",
		Now:         func() time.Time { return time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC) },
		PoolRoot:    poolRoot,
		PoolMounted: func(string) (bool, error) { return false, nil },
	}

	if err := svc.RunReason(ctx, ReasonPreTopology); err == nil {
		t.Fatal("expected RunReason to fail when its only destination is unavailable")
	}
	if _, err := os.Stat(poolDest); !os.IsNotExist(err) {
		t.Fatalf("pool destination %q must not exist: stat error = %v", poolDest, err)
	}
}

// TestService_RunTreatsUnconfirmableMountStateAsUnavailable proves a
// PoolMounted error — pool.IsMountedConfirmed's own tri-state case, a dead
// FUSE endpoint whose mount point cannot be confirmed live or gone — is
// treated as "not safe to write", never as "mounted": the destination is
// skipped exactly like a confirmed-unmounted one, not written to on the
// strength of an error that could not confirm either state.
func TestService_RunTreatsUnconfirmableMountStateAsUnavailable(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)

	poolRoot := filepath.Join(root, "mnt", "user")
	poolDest := filepath.Join(poolRoot, "hoserva-backups")

	svc := &Service{
		DB:    db,
		Paths: paths,
		Secrets: &FakeSecretSource{
			Passphrase: "backup-pass",
			HasPass:    true,
		},
		Cipher: FakeSecretCipher{},
		Destinations: []Destination{
			{ID: "pool", Path: poolDest, Enabled: true, Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}},
		},
		Hostname: "test-host",
		Version:  "0.0.0-test",
		Now:      func() time.Time { return time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC) },
		PoolRoot: poolRoot,
		PoolMounted: func(string) (bool, error) {
			return false, fmt.Errorf("stat /mnt/user: transport endpoint is not connected")
		},
	}

	if err := svc.Run(ctx); err == nil {
		t.Fatal("expected Run to fail when the pool's mount state cannot be confirmed and no other destination is configured")
	}
	if _, err := os.Stat(poolDest); !os.IsNotExist(err) {
		t.Fatalf("pool destination %q must not exist when its mount state could not be confirmed: stat error = %v", poolDest, err)
	}
}

// TestUnderPoolRoot proves the comparison is by path component, not string
// prefix (#409): "/mnt/username" shares "/mnt/user" as a string prefix but
// is a sibling directory, never a destination actually under the pool.
func TestUnderPoolRoot(t *testing.T) {
	cases := []struct {
		path string
		root string
		want bool
	}{
		{"/mnt/user", "/mnt/user", true},
		{"/mnt/user/hoserva-backups", "/mnt/user", true},
		{"/mnt/user/", "/mnt/user", true},
		{"/mnt/username", "/mnt/user", false},
		{"/mnt/username/hoserva-backups", "/mnt/user", false},
		{"/var/lib/hoserva/backups", "/mnt/user", false},
	}
	for _, tc := range cases {
		if got := underPoolRoot(tc.path, tc.root); got != tc.want {
			t.Errorf("underPoolRoot(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
		}
	}
}

// TestService_RunReasonSkipsPoolDestinationOnceGateIsClosed is #409's own
// proof that PoolWriteGate, not just the mount check, guards every
// RunReason caller — pre-import, pre-update and pre-topology alike, all of
// which call RunReason directly rather than through job.Scheduler. With
// Close already called (as job.ArraySequence.Stop calls it before it
// unmounts the pool) a write refused here never races that unmount: the
// pool destination is skipped and recorded exactly like an unmounted pool,
// while the boot destination — a separate failure domain — is written.
func TestService_RunReasonSkipsPoolDestinationOnceGateIsClosed(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)

	poolRoot := filepath.Join(root, "mnt", "user")
	poolDest := filepath.Join(poolRoot, "hoserva-backups")
	bootDest := filepath.Join(root, "boot-backups")

	gate := &PoolWriteGate{}
	if err := gate.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	now := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	svc := &Service{
		DB:    db,
		Paths: paths,
		Secrets: &FakeSecretSource{
			Passphrase: "backup-pass",
			HasPass:    true,
		},
		Cipher: FakeSecretCipher{},
		Destinations: []Destination{
			{ID: "boot", Path: bootDest, Enabled: true, Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}},
			{ID: "pool", Path: poolDest, Enabled: true, Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}},
		},
		Hostname: "test-host",
		Version:  "0.0.0-test",
		Now:      func() time.Time { return now },
		PoolRoot: poolRoot,
		// A gate that refuses admission must never even reach the mount
		// check — a mounted pool would otherwise still be written to.
		PoolMounted:   func(string) (bool, error) { return true, nil },
		PoolWriteGate: gate,
	}

	if err := svc.RunReason(ctx, ReasonPreImport); err != nil {
		t.Fatalf("RunReason: %v", err)
	}

	name := archiveName(svc.installationID(), now, ReasonPreImport, 0)
	if _, err := os.Stat(filepath.Join(bootDest, name)); err != nil {
		t.Fatalf("boot archive missing: %v", err)
	}
	if _, err := os.Stat(poolDest); !os.IsNotExist(err) {
		t.Fatalf("pool destination %q must not exist — PoolWriteGate is closed (#409): stat error = %v", poolDest, err)
	}
}

// TestService_RunWritesPoolDestinationAfterGateReopens proves Open, called
// by job.ArraySequence.Start once the pool is actually mounted again
// (#409), lets a pool-destination write proceed exactly as it would if no
// gate were wired at all.
func TestService_RunWritesPoolDestinationAfterGateReopens(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)

	poolRoot := filepath.Join(root, "mnt", "user")
	if err := os.MkdirAll(poolRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	poolDest := filepath.Join(poolRoot, "hoserva-backups")

	gate := &PoolWriteGate{}
	if err := gate.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	gate.Open()

	now := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	svc := &Service{
		DB:    db,
		Paths: paths,
		Secrets: &FakeSecretSource{
			Passphrase: "backup-pass",
			HasPass:    true,
		},
		Cipher: FakeSecretCipher{},
		Destinations: []Destination{
			{ID: "pool", Path: poolDest, Enabled: true, Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}},
		},
		Hostname:      "test-host",
		Version:       "0.0.0-test",
		Now:           func() time.Time { return now },
		PoolRoot:      poolRoot,
		PoolMounted:   func(string) (bool, error) { return true, nil },
		PoolWriteGate: gate,
	}

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	name := archiveName(svc.installationID(), now, ReasonNone, 0)
	if _, err := os.Stat(filepath.Join(poolDest, name)); err != nil {
		t.Fatalf("pool archive missing despite the gate having reopened: %v", err)
	}
}

// TestService_PoolWriteGateBlocksCloseUntilAnInFlightWriteFinishes proves
// backup.Service and job.ArraySequence's own PoolWriteGate integrate the
// way #409 requires: a write already admitted by begin — here, one
// deliberately held inside the mount check, standing in for "the mount
// check has passed and the write itself is under way" — holds Close open
// until it actually finishes, the same guarantee job.ArraySequence.Stop
// relies on before it ever unmounts the pool.
func TestService_PoolWriteGateBlocksCloseUntilAnInFlightWriteFinishes(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)

	poolRoot := filepath.Join(root, "mnt", "user")
	if err := os.MkdirAll(poolRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	poolDest := filepath.Join(poolRoot, "hoserva-backups")

	gate := &PoolWriteGate{}
	mountCheckEntered := make(chan struct{})
	releaseMountCheck := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseMountCheck) }) }
	// If an assertion below fails first, this still unblocks Run's own
	// goroutine instead of leaking it into later tests in this package.
	defer release()

	svc := &Service{
		DB:    db,
		Paths: paths,
		Secrets: &FakeSecretSource{
			Passphrase: "backup-pass",
			HasPass:    true,
		},
		Cipher: FakeSecretCipher{},
		Destinations: []Destination{
			{ID: "pool", Path: poolDest, Enabled: true, Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}},
		},
		Hostname: "test-host",
		Version:  "0.0.0-test",
		Now:      func() time.Time { return time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC) },
		PoolRoot: poolRoot,
		PoolMounted: func(string) (bool, error) {
			close(mountCheckEntered)
			<-releaseMountCheck
			return true, nil
		},
		PoolWriteGate: gate,
	}

	runDone := make(chan error, 1)
	go func() { runDone <- svc.Run(ctx) }()

	// BuildArchive/VerifyArchive run a real passphrase-based key
	// derivation before the destination loop is ever reached — several
	// seconds even on this package's own other tests (TestService_
	// RunCreatesVerifiedArchive and its siblings routinely take 5-14s) —
	// so this must give Run far longer than an in-memory handshake would
	// otherwise need.
	select {
	case <-mountCheckEntered:
	case <-time.After(20 * time.Second):
		t.Fatal("Run never reached the pool mount check")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- gate.Close(context.Background()) }()

	select {
	case <-closeDone:
		t.Fatal("Close returned while Run's own pool-destination write was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	release()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not finish after the mount check unblocked")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return after Run finished")
	}
}

func TestService_RunEncryptsArchiveForEncryptDestination(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)
	destDir := filepath.Join(root, "backups-encrypted")

	recipient, err := LoadOrGenerateRecipient(ctx, FakeSecretCipher{}, &FakeRecipientStore{}, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerateRecipient: %v", err)
	}

	now := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	svc := &Service{
		DB:    db,
		Paths: paths,
		Secrets: &FakeSecretSource{
			Passphrase: "backup-pass",
			HasPass:    true,
		},
		Cipher:    FakeSecretCipher{},
		Recipient: recipient,
		Destinations: []Destination{{
			ID:      "remote",
			Path:    destDir,
			Enabled: true,
			Encrypt: true,
			Retention: Retention{
				Daily:   7,
				Weekly:  4,
				Monthly: 6,
			},
		}},
		Hostname: "test-host",
		Version:  "0.0.0-test",
		Now:      func() time.Time { return now },
	}

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	name := archiveName(svc.installationID(), now, ReasonNone, 0)
	plainPath := filepath.Join(destDir, name)
	if _, err := os.Stat(plainPath); err == nil {
		t.Fatalf("an Encrypt destination must never receive the plaintext archive %q", plainPath)
	}

	encPath := plainPath + ".age"
	sidecarPath := encPath + identitySidecarSuffix
	if _, err := os.Stat(encPath); err != nil {
		t.Fatalf("encrypted archive missing: %v", err)
	}
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("identity sidecar missing: %v", err)
	}

	got, err := decryptArchiveWithPassphrase(encPath, sidecarPath, "backup-pass")
	if err != nil {
		t.Fatalf("decryptArchiveWithPassphrase: %v", err)
	}
	// A decrypted archive is still a plain tar.zst — unpackArchive must
	// read it back the same way a local archive is verified.
	verifyDir := t.TempDir()
	decrypted := filepath.Join(verifyDir, "decrypted.tar.zst")
	if err := os.WriteFile(decrypted, got, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unpackArchive(decrypted, filepath.Join(verifyDir, "out")); err != nil {
		t.Fatalf("unpackArchive on decrypted content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, "out", "state.db")); err != nil {
		t.Fatalf("state.db missing from decrypted archive: %v", err)
	}
}

// TestService_RunEmbedsIdentityAgeInArchiveWrittenToDestination proves
// criterion 3 ("the private identity is embedded in every archive") holds
// for the archive Service.Run itself writes to a destination — the nightly
// config-backup chain this issue's "Reachable via" names — not only for
// BuildArchive in isolation or for ExportConfig's on-demand path (covered
// separately by internal/api's own TestExportConfig test). It uses a plain,
// unencrypted local destination so the file written to disk is exactly the
// tar.zst BuildArchive staged, unpacks it, and decrypts identity.age with
// the backup passphrase through the same scrypt path secrets.age uses. A
// Service.Run that stopped passing WithRecipient(s.Recipient) to
// BuildArchive would leave identity.age out of the staged archive
// entirely, and the os.ReadFile below would fail before the decrypt is
// ever reached.
func TestService_RunEmbedsIdentityAgeInArchiveWrittenToDestination(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)
	destDir := filepath.Join(root, "backups")

	recipient, err := LoadOrGenerateRecipient(ctx, FakeSecretCipher{}, &FakeRecipientStore{}, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerateRecipient: %v", err)
	}

	now := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	svc := &Service{
		DB:    db,
		Paths: paths,
		Secrets: &FakeSecretSource{
			Passphrase: "backup-pass",
			HasPass:    true,
		},
		Cipher:    FakeSecretCipher{},
		Recipient: recipient,
		Destinations: []Destination{{
			ID:      "local",
			Path:    destDir,
			Enabled: true,
			Retention: Retention{
				Daily:   7,
				Weekly:  4,
				Monthly: 6,
			},
		}},
		Hostname: "test-host",
		Version:  "0.0.0-test",
		Now:      func() time.Time { return now },
	}

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	archivePath := filepath.Join(destDir, archiveName(svc.installationID(), now, ReasonNone, 0))
	verifyDir := t.TempDir()
	if err := unpackArchive(archivePath, verifyDir); err != nil {
		t.Fatalf("unpackArchive: %v", err)
	}

	identityAge, err := os.ReadFile(filepath.Join(verifyDir, "identity.age"))
	if err != nil {
		t.Fatalf("archive written by Run has no identity.age: %v", err)
	}
	scryptIdentity, err := age.NewScryptIdentity("backup-pass")
	if err != nil {
		t.Fatalf("creating scrypt identity: %v", err)
	}
	r, err := age.Decrypt(bytes.NewReader(identityAge), scryptIdentity)
	if err != nil {
		t.Fatalf("decrypting identity.age with the backup passphrase: %v", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading decrypted identity.age: %v", err)
	}
	if string(plain) != recipient.Identity {
		t.Fatalf("decrypted identity.age = %q, want %q", plain, recipient.Identity)
	}
}

func TestService_RunFailsClosedWhenEncryptRequestedWithoutPassphrase(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)
	destDir := filepath.Join(root, "backups-encrypted")

	recipient, err := LoadOrGenerateRecipient(ctx, FakeSecretCipher{}, &FakeRecipientStore{}, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerateRecipient: %v", err)
	}

	svc := &Service{
		DB:        db,
		Paths:     paths,
		Secrets:   &FakeSecretSource{HasPass: false},
		Cipher:    FakeSecretCipher{},
		Recipient: recipient,
		Destinations: []Destination{{
			ID:      "remote",
			Path:    destDir,
			Enabled: true,
			Encrypt: true,
		}},
		Hostname: "test-host",
		Version:  "0.0.0-test",
		Now:      func() time.Time { return time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC) },
	}

	if err := svc.Run(ctx); err == nil {
		t.Fatal("expected Run to fail when Encrypt is requested with no backup passphrase configured")
	}
	if entries, _ := os.ReadDir(destDir); len(entries) != 0 {
		t.Fatalf("expected no files written to the destination, found %d", len(entries))
	}
}

func TestService_RunFailsClosedWhenEncryptRequestedWithoutRecipient(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	paths, root := testLayout(t)
	destDir := filepath.Join(root, "backups-encrypted")

	svc := &Service{
		DB:      db,
		Paths:   paths,
		Secrets: &FakeSecretSource{Passphrase: "backup-pass", HasPass: true},
		Cipher:  FakeSecretCipher{},
		Destinations: []Destination{{
			ID:      "remote",
			Path:    destDir,
			Enabled: true,
			Encrypt: true,
		}},
		Hostname: "test-host",
		Version:  "0.0.0-test",
		Now:      func() time.Time { return time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC) },
	}

	if err := svc.Run(ctx); err == nil {
		t.Fatal("expected Run to fail when Encrypt is requested with no onboarding recipient available")
	}
}

// runConcurrentServices starts len(svcs) Service.Run calls together (a
// closed start gate maximizes their overlap) and returns each call's
// error, in order.
func runConcurrentServices(svcs []*Service) []error {
	errs := make([]error, len(svcs))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, svc := range svcs {
		wg.Add(1)
		go func(i int, svc *Service) {
			defer wg.Done()
			<-start
			errs[i] = svc.Run(context.Background())
		}(i, svc)
	}
	close(start)
	wg.Wait()
	return errs
}

// TestService_RunConcurrentRunsDoNotShareArchivePath proves two Run calls
// that land on the same minute (issue #405) each get their own destination
// archive rather than racing on a shared, minute-keyed temp path: before
// the fix, every run computes the identical filepath.Join(os.TempDir(),
// archiveName(now)), so a later run's packArchive rename or a finishing
// run's defer os.Remove can hand one destination another host's archive,
// or fail one run outright with "no such file or directory". TMPDIR is
// pointed at a directory this test owns exclusively (created before the
// Setenv so the test's own t.TempDir() scratch space is unaffected), so
// the final os.ReadDir proves Run leaves nothing behind — on the unfixed
// code that assertion fails too, since two runs finishing seconds apart
// still overwrite rather than clean up after each other's shared path.
func TestService_RunConcurrentRunsDoNotShareArchivePath(t *testing.T) {
	const runs = 8
	now := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	root := t.TempDir()

	type run struct {
		host      string
		destDir   string
		verifyDir string
	}
	plan := make([]run, runs)
	svcs := make([]*Service, runs)
	for i := 0; i < runs; i++ {
		host := fmt.Sprintf("host-%d", i)
		destDir := filepath.Join(root, host)
		db := openTestDB(t)
		paths, _ := testLayout(t)
		plan[i] = run{host: host, destDir: destDir, verifyDir: t.TempDir()}
		svcs[i] = &Service{
			DB:    db,
			Paths: paths,
			Secrets: &FakeSecretSource{
				Passphrase: "backup-pass",
				HasPass:    true,
			},
			Cipher: FakeSecretCipher{},
			Destinations: []Destination{{
				ID:      host,
				Path:    destDir,
				Enabled: true,
				Retention: Retention{
					Daily:   7,
					Weekly:  4,
					Monthly: 6,
				},
			}},
			Hostname: host,
			Version:  "0.0.0-test",
			Now:      func() time.Time { return now },
		}
	}

	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	for i, err := range runConcurrentServices(svcs) {
		if err != nil {
			t.Fatalf("Run for %q: %v", plan[i].host, err)
		}
	}

	for i, r := range plan {
		archivePath := filepath.Join(r.destDir, archiveName(svcs[i].installationID(), now, ReasonNone, 0))
		if err := unpackArchive(archivePath, r.verifyDir); err != nil {
			t.Fatalf("unpackArchive for %q: %v", r.host, err)
		}
		manifest, err := readManifest(filepath.Join(r.verifyDir, "manifest.json"))
		if err != nil {
			t.Fatalf("readManifest for %q: %v", r.host, err)
		}
		if manifest.Host != r.host {
			t.Fatalf("destination %q received host %q's archive, want %q", r.destDir, manifest.Host, r.host)
		}
	}

	leftover, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatalf("reading TMPDIR: %v", err)
	}
	if len(leftover) != 0 {
		names := make([]string, len(leftover))
		for i, e := range leftover {
			names[i] = e.Name()
		}
		t.Fatalf("TMPDIR not clean after concurrent runs: %v", names)
	}
}

// TestService_RunConcurrentEncryptRunsDoNotShareArchivePath is the same
// race, exercised through the Encrypt destination path: buildArtifacts
// derives the encrypted archive and identity sidecar paths from
// archivePath itself (archivePath+".age", +identitySidecarSuffix), so if
// archivePath is shared, those derived paths are too.
func TestService_RunConcurrentEncryptRunsDoNotShareArchivePath(t *testing.T) {
	// Kept small: each run's buildArtifacts pays a real scrypt cost, and
	// this test is exercised with -race -count=20.
	const runs = 3
	now := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	root := t.TempDir()

	recipient, err := LoadOrGenerateRecipient(context.Background(), FakeSecretCipher{}, &FakeRecipientStore{}, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerateRecipient: %v", err)
	}

	type run struct {
		host      string
		destDir   string
		verifyDir string
	}
	plan := make([]run, runs)
	svcs := make([]*Service, runs)
	for i := 0; i < runs; i++ {
		host := fmt.Sprintf("host-%d", i)
		destDir := filepath.Join(root, host)
		db := openTestDB(t)
		paths, _ := testLayout(t)
		plan[i] = run{host: host, destDir: destDir, verifyDir: t.TempDir()}
		svcs[i] = &Service{
			DB:    db,
			Paths: paths,
			Secrets: &FakeSecretSource{
				Passphrase: "backup-pass",
				HasPass:    true,
			},
			Cipher:    FakeSecretCipher{},
			Recipient: recipient,
			Destinations: []Destination{{
				ID:      host,
				Path:    destDir,
				Enabled: true,
				Encrypt: true,
				Retention: Retention{
					Daily:   7,
					Weekly:  4,
					Monthly: 6,
				},
			}},
			Hostname: host,
			Version:  "0.0.0-test",
			Now:      func() time.Time { return now },
		}
	}

	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	for i, err := range runConcurrentServices(svcs) {
		if err != nil {
			t.Fatalf("Run for %q: %v", plan[i].host, err)
		}
	}

	for i, r := range plan {
		encPath := filepath.Join(r.destDir, archiveName(svcs[i].installationID(), now, ReasonNone, 0)+".age")
		sidecarPath := encPath + identitySidecarSuffix
		got, err := decryptArchiveWithPassphrase(encPath, sidecarPath, "backup-pass")
		if err != nil {
			t.Fatalf("decryptArchiveWithPassphrase for %q: %v", r.host, err)
		}
		decrypted := filepath.Join(r.verifyDir, "decrypted.tar.zst")
		if err := os.WriteFile(decrypted, got, 0o600); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(r.verifyDir, "out")
		if err := unpackArchive(decrypted, out); err != nil {
			t.Fatalf("unpackArchive for %q: %v", r.host, err)
		}
		manifest, err := readManifest(filepath.Join(out, "manifest.json"))
		if err != nil {
			t.Fatalf("readManifest for %q: %v", r.host, err)
		}
		if manifest.Host != r.host {
			t.Fatalf("destination %q received host %q's archive, want %q", r.destDir, manifest.Host, r.host)
		}
	}

	leftover, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatalf("reading TMPDIR: %v", err)
	}
	if len(leftover) != 0 {
		names := make([]string, len(leftover))
		for i, e := range leftover {
			names[i] = e.Name()
		}
		t.Fatalf("TMPDIR not clean after concurrent runs: %v", names)
	}
}

func TestDefaultDestinations(t *testing.T) {
	dests := DefaultDestinations()
	if len(dests) != 2 {
		t.Fatalf("expected 2 default destinations, got %d", len(dests))
	}
	if dests[0].Path != DefaultBootDestination {
		t.Fatalf("boot destination: got %q", dests[0].Path)
	}
	if dests[1].Path != DefaultPoolDestination {
		t.Fatalf("pool destination: got %q", dests[1].Path)
	}
}

func TestRetentionPrune(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	names := []string{
		"hoserva-config-2026-09-14T03-00.tar.zst",
		"hoserva-config-2026-09-13T03-00.tar.zst",
		"hoserva-config-2026-09-12T03-00.tar.zst",
		"hoserva-config-2026-09-11T03-00.tar.zst",
		"hoserva-config-2026-09-10T03-00.tar.zst",
		"hoserva-config-2026-09-09T03-00.tar.zst",
		"hoserva-config-2026-09-08T03-00.tar.zst",
		"hoserva-config-2026-09-07T03-00.tar.zst",
		"hoserva-config-2026-09-01T03-00.tar.zst",
	}
	for i, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := now.AddDate(0, 0, -i)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	dest := Destination{
		Path: dir,
		Retention: Retention{
			Daily:   7,
			Weekly:  4,
			Monthly: 6,
		},
	}
	justWritten := names[0]
	if err := pruneDestination(dest, archiveOwner{legacy: true}, now, justWritten); err != nil {
		t.Fatalf("pruneDestination: %v", err)
	}

	remaining, err := listArchives(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) == 0 {
		t.Fatal("expected some archives to remain")
	}
	for _, e := range remaining {
		if e.name == "hoserva-config-2026-09-01T03-00.tar.zst" && len(remaining) < len(names) {
			// oldest may be pruned depending on weekly/monthly buckets
			continue
		}
	}
}

func TestRetentionPrune_RemovesOrphanedIdentitySidecar(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	pruned := "hoserva-config-2020-01-01T03-00.tar.zst.age"
	kept := "hoserva-config-2026-09-14T03-00.tar.zst.age"
	for _, name := range []string{pruned, kept} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+identitySidecarSuffix), []byte("identity"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldMod := now.AddDate(-1, 0, 0)
	if err := os.Chtimes(filepath.Join(dir, pruned), oldMod, oldMod); err != nil {
		t.Fatal(err)
	}

	dest := Destination{Path: dir, Retention: Retention{Daily: 1, Weekly: 1, Monthly: 1}}
	if err := pruneDestination(dest, archiveOwner{legacy: true}, now, kept); err != nil {
		t.Fatalf("pruneDestination: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, pruned)); !os.IsNotExist(err) {
		t.Fatalf("expected pruned archive to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, pruned+identitySidecarSuffix)); !os.IsNotExist(err) {
		t.Fatalf("expected pruned archive's identity sidecar to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
		t.Fatalf("kept archive missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, kept+identitySidecarSuffix)); err != nil {
		t.Fatalf("kept archive's identity sidecar missing: %v", err)
	}
}

const testInstallation = "0123456789ab"

func TestArchiveName_MatchesRetentionPattern(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 42, 7, 0, time.UTC)
	name := archiveName(testInstallation, now, ReasonNone, 0)
	want := "hoserva-config-0123456789ab-2026-09-14T15-42-07.tar.zst"
	if name != want {
		t.Fatalf("archiveName = %q, want %q", name, want)
	}
	if archiveNamePattern.FindStringSubmatch(name) == nil {
		t.Fatalf("%q does not match archiveNamePattern", name)
	}
}

// TestArchiveName_LegacyMinuteResolutionStillMatches proves an archive
// written before #401 — minute resolution, no reason marker — still
// parses as a retention candidate: a filename already on a destination
// from before this fix must keep pruning exactly as it always did.
func TestArchiveName_LegacyMinuteResolutionStillMatches(t *testing.T) {
	name := "hoserva-config-2026-09-14T15-42.tar.zst"
	m := archiveNamePattern.FindStringSubmatch(name)
	if m == nil {
		t.Fatalf("%q does not match archiveNamePattern", name)
	}
	if m[1] != "" || m[3] != "" {
		t.Fatalf("legacy archive %q parsed installation %q and reason %q, want none of either", name, m[1], m[3])
	}
}

// TestArchiveName_MarksPreChangeReason proves a pre-change archive name
// carries its reason and still matches archiveNamePattern, with a
// collision suffix parsed correctly alongside it (#401).
func TestArchiveName_MarksPreChangeReason(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 42, 7, 0, time.UTC)
	name := archiveName(testInstallation, now, ReasonPreImport, 2)
	want := "hoserva-config-0123456789ab-2026-09-14T15-42-07-2.pre-import.tar.zst"
	if name != want {
		t.Fatalf("archiveName = %q, want %q", name, want)
	}
	m := archiveNamePattern.FindStringSubmatch(name)
	if m == nil {
		t.Fatalf("%q does not match archiveNamePattern", name)
	}
	if m[1] != testInstallation || m[3] != "pre-import" {
		t.Fatalf("archiveNamePattern parsed installation %q and reason %q, want %q and pre-import", m[1], m[3], testInstallation)
	}
}

// newSameDestinationService builds a Service pointed at destDir with its
// own database and source tree, so two calls with different clocks each
// produce a genuine archive rather than reusing state across runs — the
// way two separate ImportConfig requests on the same day each build their
// own.
func newSameDestinationService(t *testing.T, recipient *Recipient, destDir, hostname string, now time.Time) *Service {
	t.Helper()
	db := openTestDB(t)
	paths, _ := testLayout(t)
	return &Service{
		DB:    db,
		Paths: paths,
		Secrets: &FakeSecretSource{
			Passphrase: "backup-pass",
			HasPass:    true,
		},
		Cipher: FakeSecretCipher{},
		Destinations: []Destination{{
			ID:      "local",
			Path:    destDir,
			Enabled: true,
			Retention: Retention{
				Daily:   7,
				Weekly:  4,
				Monthly: 6,
			},
		}},
		Recipient: recipient,
		Hostname:  hostname,
		Version:   "0.0.0-test",
		Now:       func() time.Time { return now },
	}
}

func newTestRecipient(t *testing.T) *Recipient {
	t.Helper()
	recipient, err := LoadOrGenerateRecipient(context.Background(), FakeSecretCipher{}, &FakeRecipientStore{}, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerateRecipient: %v", err)
	}
	return recipient
}

// TestService_RunReasonSurvivesSameDayOrdinaryBackup reproduces #401: a
// pre-import safety backup taken earlier in the day must survive a later
// same-day backup's retention prune. Before the fix, retentionKeepers'
// daily tier keeps only the newest archive per calendar day, so the
// second run — an ordinary backup, exactly what a second import's own
// pre-import backup or a same-day self-update produces — deletes the
// first run's archive: the only copy of the state from before the first
// destructive change. Reverting retentionKeepers' pre-change loop
// (destination.go) makes this test fail with the first archive missing.
func TestService_RunReasonSurvivesSameDayOrdinaryBackup(t *testing.T) {
	ctx := context.Background()
	destDir := t.TempDir()
	recipient := newTestRecipient(t)

	firstRun := time.Date(2026, 9, 14, 15, 4, 0, 0, time.UTC)
	first := newSameDestinationService(t, recipient, destDir, "host-a", firstRun)
	if err := first.RunReason(ctx, ReasonPreImport); err != nil {
		t.Fatalf("first RunReason: %v", err)
	}
	firstName := archiveName(first.installationID(), firstRun, ReasonPreImport, 0)
	firstPath := filepath.Join(destDir, firstName)
	if _, err := os.Stat(firstPath); err != nil {
		t.Fatalf("first pre-import archive missing right after it was written: %v", err)
	}

	secondRun := firstRun.Add(6 * time.Minute)
	second := newSameDestinationService(t, recipient, destDir, "host-b", secondRun)
	if err := second.RunReason(ctx, ReasonPreImport); err != nil {
		t.Fatalf("second RunReason: %v", err)
	}

	if _, err := os.Stat(firstPath); err != nil {
		t.Fatalf("earlier same-day pre-import archive was pruned by the later backup: %v", err)
	}
}

// TestService_RunTwiceInSameMinuteProducesDistinctArchives proves the
// second half of #401: two Run calls whose clock lands in the very same
// second — the tightest case of "the same minute" the original,
// minute-only archiveName always collided on — no longer collide. Both
// runs are marked pre-import, the realistic case of a double-submitted
// import request, so retention's own daily-tier collapsing (by design,
// one ordinary archive survives per calendar day) never enters into it:
// this test is purely about the write no longer overwriting. The first
// archive's own content (its manifest host) must still be readable
// afterward, proving the second run did not overwrite it — before the
// fix, both runs computed the identical archiveName(now) and the second
// run's os.Rename onto that shared final path silently replaced the
// first run's file.
func TestService_RunTwiceInSameMinuteProducesDistinctArchives(t *testing.T) {
	ctx := context.Background()
	destDir := t.TempDir()
	recipient := newTestRecipient(t)

	now := time.Date(2026, 9, 14, 3, 0, 30, 0, time.UTC)
	first := newSameDestinationService(t, recipient, destDir, "host-a", now)
	if err := first.RunReason(ctx, ReasonPreImport); err != nil {
		t.Fatalf("first RunReason: %v", err)
	}
	second := newSameDestinationService(t, recipient, destDir, "host-b", now)
	if err := second.RunReason(ctx, ReasonPreImport); err != nil {
		t.Fatalf("second RunReason: %v", err)
	}

	entries, err := listArchives(destDir)
	if err != nil {
		t.Fatalf("listArchives: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.name
		}
		t.Fatalf("expected 2 distinct archives, got %d: %v", len(entries), names)
	}

	firstName := archiveName(first.installationID(), now, ReasonPreImport, 0)
	firstPath := filepath.Join(destDir, firstName)
	verifyDir := t.TempDir()
	if err := unpackArchive(firstPath, verifyDir); err != nil {
		t.Fatalf("unpackArchive on the first run's own archive: %v", err)
	}
	manifest, err := readManifest(filepath.Join(verifyDir, "manifest.json"))
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if manifest.Host != "host-a" {
		t.Fatalf("first archive's manifest host = %q, want %q (second run overwrote it)", manifest.Host, "host-a")
	}
}

// TestRetentionPrune_BoundsPreChangeArchives proves the third acceptance
// criterion: pre-change archives are kept up to preChangeKeepCount on top
// of the daily/weekly/monthly tiers, and the oldest pre-change archive
// beyond that bound is pruned: pre-change archives never take part in the
// ordinary tiers. Retention is set to keep nothing in any tier, so only
// the pre-change bound is under test.
func TestRetentionPrune_BoundsPreChangeArchives(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	var names []string
	for i := 0; i < 6; i++ {
		day := now.AddDate(0, 0, -i)
		name := archiveName(testInstallation, day, ReasonPreImport, 0)
		names = append(names, name)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, day, day); err != nil {
			t.Fatal(err)
		}
	}

	dest := Destination{Path: dir, Retention: Retention{Daily: 0, Weekly: 0, Monthly: 0}}
	if err := pruneDestination(dest, archiveOwner{installation: testInstallation}, now, names[0]); err != nil {
		t.Fatalf("pruneDestination: %v", err)
	}

	remaining, err := listArchives(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != preChangeKeepCount {
		got := make([]string, len(remaining))
		for i, e := range remaining {
			got[i] = e.name
		}
		t.Fatalf("expected %d surviving pre-change archives, got %d: %v", preChangeKeepCount, len(remaining), got)
	}
	for _, e := range remaining {
		if e.name == names[5] {
			t.Fatalf("oldest pre-change archive %q beyond the bound of %d should have been pruned", names[5], preChangeKeepCount)
		}
	}
	for i := 0; i < preChangeKeepCount; i++ {
		if _, err := os.Stat(filepath.Join(dir, names[i])); err != nil {
			t.Fatalf("expected pre-change archive %q to survive: %v", names[i], err)
		}
	}
}

// TestRetentionPrune_PreChangeArchiveNeverEvictsAnOrdinaryOne reproduces
// #478: with Daily: 1, a pre-update archive newer than the same day's
// ordinary archive used to win that day's daily slot, so its prune deleted
// the ordinary archive — the same deletion as a pre-update backup taken
// right after a scheduled one, no concurrency needed. Restoring the
// pre-change archives to the shared byDay/byWeek/byMonth loops in
// retentionKeepers (destination.go) makes this fail with the ordinary
// archive missing.
func TestRetentionPrune_PreChangeArchiveNeverEvictsAnOrdinaryOne(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 14, 3, 30, 0, 0, time.UTC)

	ordinary := archiveName(testInstallation, now.Add(-10*time.Minute), ReasonNone, 0)
	preUpdate := archiveName(testInstallation, now, ReasonPreUpdate, 0)
	for name, mod := range map[string]time.Time{ordinary: now.Add(-10 * time.Minute), preUpdate: now} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	dest := Destination{Path: dir, Retention: Retention{Daily: 1}}
	if err := pruneDestination(dest, archiveOwner{installation: testInstallation}, now, preUpdate); err != nil {
		t.Fatalf("pruneDestination: %v", err)
	}
	for _, name := range []string{ordinary, preUpdate} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("archive %q was pruned: %v", name, err)
		}
	}
}

// TestRetentionPrune_PreChangeArchivesDoNotOccupyOrdinarySlots proves the
// converse: a run of pre-change archives is not counted towards the
// ordinary daily tier, so Daily: 2 still keeps the two newest ordinary
// days beside the pre-change archives.
func TestRetentionPrune_PreChangeArchivesDoNotOccupyOrdinarySlots(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	type spec struct {
		reason Reason
		mod    time.Time
	}
	var names []string
	specs := []spec{
		{ReasonPreImport, now},
		{ReasonNone, now.Add(-1 * time.Hour)},
		{ReasonPreUpdate, now.AddDate(0, 0, -1)},
		{ReasonNone, now.AddDate(0, 0, -1).Add(-1 * time.Hour)},
		{ReasonNone, now.AddDate(0, 0, -2)},
	}
	for _, s := range specs {
		name := archiveName(testInstallation, s.mod, s.reason, 0)
		names = append(names, name)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, s.mod, s.mod); err != nil {
			t.Fatal(err)
		}
	}

	dest := Destination{Path: dir, Retention: Retention{Daily: 2}}
	if err := pruneDestination(dest, archiveOwner{installation: testInstallation}, now, names[0]); err != nil {
		t.Fatalf("pruneDestination: %v", err)
	}
	for i, want := range []bool{true, true, true, true, false} {
		_, err := os.Stat(filepath.Join(dir, names[i]))
		if (err == nil) != want {
			t.Fatalf("archive %q kept = %v, want %v", names[i], err == nil, want)
		}
	}
}

// pendingMigrationService builds a Service whose pool is mounted — the
// state an adoption leaves it in, read-only — with a fake migration state.
func pendingMigrationService(t *testing.T, dests func(poolDest, bootDest string) []Destination, unfinished func(context.Context) (bool, error)) (svc *Service, poolDest, bootDest string, now time.Time) {
	t.Helper()
	db := openTestDB(t)
	paths, root := testLayout(t)
	poolRoot := filepath.Join(root, "mnt", "user")
	if err := os.MkdirAll(poolRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	poolDest = filepath.Join(poolRoot, "hoserva-backups")
	bootDest = filepath.Join(root, "boot-backups")
	now = time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	svc = &Service{
		DB:                  db,
		Paths:               paths,
		Secrets:             &FakeSecretSource{Passphrase: "backup-pass", HasPass: true},
		Cipher:              FakeSecretCipher{},
		Destinations:        dests(poolDest, bootDest),
		Hostname:            "test-host",
		Version:             "0.0.0-test",
		Now:                 func() time.Time { return now },
		PoolRoot:            poolRoot,
		PoolMounted:         func(string) (bool, error) { return true, nil },
		MigrationUnfinished: unfinished,
	}
	return svc, poolDest, bootDest, now
}

func poolAndBoot(poolDest, bootDest string) []Destination {
	retention := Retention{Daily: 7, Weekly: 4, Monthly: 6}
	return []Destination{
		{ID: "boot", Path: bootDest, Enabled: true, Retention: retention},
		{ID: "pool", Path: poolDest, Enabled: true, Retention: retention},
	}
}

// TestService_RunSkipsPoolDestinationWhileAMigrationIsUnfinished is #639: the
// pool is mounted but read-only until the point of no return (doc 05 §4), so
// the pool destination is skipped with a named reason instead of failing on
// EROFS, and the boot destination is written as usual.
func TestService_RunSkipsPoolDestinationWhileAMigrationIsUnfinished(t *testing.T) {
	var logs []string
	svc, poolDest, bootDest, now := pendingMigrationService(t, poolAndBoot,
		func(context.Context) (bool, error) { return true, nil })
	svc.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(poolDest); !os.IsNotExist(err) {
		t.Fatalf("pool destination %q must not be touched while a migration is unfinished: stat error = %v", poolDest, err)
	}
	if _, err := os.Stat(filepath.Join(bootDest, archiveName(svc.installationID(), now, ReasonNone, 0))); err != nil {
		t.Fatalf("boot archive missing: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], `"pool"`) || !strings.Contains(logs[0], "migration is finished") {
		t.Fatalf("logs = %q, want one line naming the pool destination and the pending migration", logs)
	}
}

func TestService_RunFailsNamingTheMigrationWhenThePoolIsTheOnlyDestination(t *testing.T) {
	svc, poolDest, _, _ := pendingMigrationService(t, func(poolDest, _ string) []Destination { return poolAndBoot(poolDest, "")[1:] },
		func(context.Context) (bool, error) { return true, nil })

	err := svc.Run(context.Background())
	if err == nil {
		t.Fatal("Run reported success with nothing written")
	}
	if !strings.Contains(err.Error(), "every enabled destination was skipped") || !strings.Contains(err.Error(), "migration is finished") {
		t.Fatalf("error = %v, want it to say nothing was written and why", err)
	}
	if _, statErr := os.Stat(poolDest); !os.IsNotExist(statErr) {
		t.Fatalf("pool destination %q was touched: %v", poolDest, statErr)
	}
}

// A migration state that cannot be read is no proof there is none: the pool
// is not written, exactly as when the migration is known to be pending.
func TestService_RunDoesNotWriteThePoolWhenTheMigrationStateCannotBeRead(t *testing.T) {
	svc, poolDest, bootDest, now := pendingMigrationService(t, poolAndBoot,
		func(context.Context) (bool, error) { return false, errors.New("database is locked") })

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(poolDest); !os.IsNotExist(err) {
		t.Fatalf("pool destination %q written although the migration state was unreadable: %v", poolDest, err)
	}
	if _, err := os.Stat(filepath.Join(bootDest, archiveName(svc.installationID(), now, ReasonNone, 0))); err != nil {
		t.Fatalf("boot archive missing: %v", err)
	}

	only := func(poolDest, _ string) []Destination { return poolAndBoot(poolDest, "")[1:] }
	svc, _, _, _ = pendingMigrationService(t, only, func(context.Context) (bool, error) { return false, errors.New("database is locked") })
	if err := svc.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("Run = %v, want a failure carrying the read error", err)
	}
}

func TestService_RunWritesThePoolOnceTheMigrationIsFinished(t *testing.T) {
	svc, poolDest, _, now := pendingMigrationService(t, poolAndBoot,
		func(context.Context) (bool, error) { return false, nil })

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(poolDest, archiveName(svc.installationID(), now, ReasonNone, 0))); err != nil {
		t.Fatalf("pool archive missing with no migration pending: %v", err)
	}
}

func TestService_AdmitWriteRefusesOnlyAWriteToThePool(t *testing.T) {
	svc, poolDest, _, _ := pendingMigrationService(t, poolAndBoot,
		func(context.Context) (bool, error) { return true, nil })
	release, why := svc.admitDestination(context.Background(), Destination{ID: "pool", Path: poolDest})
	if why != "" {
		t.Fatalf("a read of the pool refused during a migration: %s", why)
	}
	release()
	if _, why := svc.admitWrite(context.Background(), Destination{ID: "pool", Path: poolDest}); !strings.Contains(why, "migration is finished") {
		t.Fatalf("a write to the pool admitted during a migration: %q", why)
	}
	if release, why := svc.admitWrite(context.Background(), Destination{ID: "boot", Path: filepath.Join(filepath.Dir(poolDest), "..", "boot")}); why != "" {
		t.Fatalf("a write outside the pool refused during a migration: %s", why)
	} else {
		release()
	}
}
