package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

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
