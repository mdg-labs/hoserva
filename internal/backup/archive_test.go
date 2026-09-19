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
	data, err := buildSecretsAge(ctx, src, FakeSecretCipher{}, envs)
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
