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

func TestBuildArchive_StateDBPassesIntegrityCheck(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES (1, 'live');"); err != nil {
		t.Fatal(err)
	}

	configRoot := filepath.Join(dir, "config")
	if err := os.MkdirAll(configRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "snapraid.conf"), []byte("content /\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	staging := filepath.Join(dir, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err = BuildArchive(ctx, db, Paths{ConfigRoot: configRoot}, nil, nil, "host", "test", time.Now(), staging)
	if err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}

	snapshot, err := sql.Open("sqlite", filepath.Join(staging, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	var check string
	if err := snapshot.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil {
		t.Fatal(err)
	}
	if check != "ok" {
		t.Fatalf("integrity_check: %s", check)
	}
	var count int
	if err := snapshot.QueryRow("SELECT COUNT(*) FROM t").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one row in snapshot, got %d", count)
	}
}

func TestSecretsAge_RoundTrip(t *testing.T) {
	ctx := context.Background()
	src := &FakeSecretSource{
		Passphrase: "test-pass",
		HasPass:    true,
		Secrets: []DatabaseSecret{{
			Table:      "notify_channels",
			Column:     "secret",
			RowID:      "ch1",
			Ciphertext: []byte{0x5a},
		}},
	}
	envs := []StackEnv{{Stack: "plex", Body: []byte("TOKEN=x\n")}}
	data, err := buildSecretsAge(ctx, src, FakeSecretCipher{}, envs, "")
	if err != nil {
		t.Fatalf("buildSecretsAge: %v", err)
	}
	payload, err := decryptSecretsAge(data, "test-pass")
	if err != nil {
		t.Fatalf("decryptSecretsAge: %v", err)
	}
	if len(payload.Database) != 1 || payload.Database[0].Table != "notify_channels" {
		t.Fatalf("unexpected database secrets: %+v", payload.Database)
	}
	if len(payload.Stacks) != 1 || payload.Stacks[0].Stack != "plex" {
		t.Fatalf("unexpected stack envs: %+v", payload.Stacks)
	}
}

func TestBuildArchive_NestedCustomConf(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY);"); err != nil {
		t.Fatal(err)
	}

	configRoot := filepath.Join(dir, "config", "samba")
	if err := os.MkdirAll(configRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(configRoot, "smb.custom.conf")
	if err := os.WriteFile(custom, []byte("guest ok = yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	staging := filepath.Join(dir, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildArchive(ctx, db, Paths{ConfigRoot: filepath.Join(dir, "config")}, nil, nil, "host", "test", time.Now(), staging); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	got := filepath.Join(staging, "custom", "samba", "smb.custom.conf")
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("nested custom file missing: %v", err)
	}
}

func TestBuildArchive_SavesHostFilesUnderHostAndTheArchiveStillVerifies(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY);"); err != nil {
		t.Fatal(err)
	}
	etc := filepath.Join(dir, "etc")
	if err := os.MkdirAll(filepath.Join(etc, "samba"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "samba", "smb.conf"), []byte("[global]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	staging := filepath.Join(dir, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := BuildArchive(ctx, db, Paths{}, nil, nil, "host", "test", time.Now(), staging, WithHostFiles(etc, []string{"samba/smb.conf"}))
	if err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	if _, ok := m.Checksums["host/samba/smb.conf"]; !ok {
		t.Fatalf("checksums %v lack the host file", m.Checksums)
	}
	archive := filepath.Join(dir, "a.tar.zst")
	if err := packArchive(staging, archive); err != nil {
		t.Fatal(err)
	}
	tree, err := ExtractVerifiedArchive(archive)
	if err != nil {
		t.Fatalf("an archive with host files does not verify: %v", err)
	}
	defer func() { _ = os.RemoveAll(tree) }()
	if got, err := os.ReadFile(filepath.Join(tree, "host", "samba", "smb.conf")); err != nil || string(got) != "[global]\n" {
		t.Fatalf("host/samba/smb.conf = %q, %v", got, err)
	}
}

// A host file the caller is about to replace that cannot be saved stops the
// archive: no archive is better than one that looks like a backup and lacks
// the file.
func TestBuildArchive_AHostFileThatCannotBeSavedFailsTheBuild(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY);"); err != nil {
		t.Fatal(err)
	}
	etc := filepath.Join(dir, "etc")
	if err := os.MkdirAll(filepath.Join(etc, "samba", "smb.conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"samba/smb.conf", "exports", "../escape"} {
		staging := filepath.Join(t.TempDir(), "staging")
		if err := os.MkdirAll(staging, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildArchive(ctx, db, Paths{}, nil, nil, "host", "test", time.Now(), staging, WithHostFiles(etc, []string{rel})); err == nil {
			t.Errorf("BuildArchive with unsavable host file %q succeeded", rel)
		}
	}
}
