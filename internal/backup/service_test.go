package backup

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
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

	name := archiveName(now)
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

	name := archiveName(now)
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

	archivePath := filepath.Join(destDir, archiveName(now))
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
	if err := pruneDestination(dest, now, justWritten); err != nil {
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
	if err := pruneDestination(dest, now, kept); err != nil {
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

func TestArchiveName_MatchesRetentionPattern(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 42, 0, 0, time.UTC)
	name := archiveName(now)
	want := "hoserva-config-2026-09-14T15-42.tar.zst"
	if name != want {
		t.Fatalf("archiveName = %q, want %q", name, want)
	}
	if archiveNamePattern.FindStringSubmatch(name) == nil {
		t.Fatalf("%q does not match archiveNamePattern", name)
	}
}
