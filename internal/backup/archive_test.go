package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
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

const machineKeyBytes = "0123456789abcdef0123456789abcdef-machine-key-material"

func buildKeyTestArchive(t *testing.T, paths Paths, opts ...ArchiveOption) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY);"); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(dir, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildArchive(context.Background(), db, paths, nil, nil, "host", "test", time.Now(), staging, opts...); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	return staging
}

func requireNoMachineKey(t *testing.T, tree string) {
	t.Helper()
	err := filepath.WalkDir(tree, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), machineKeyBytes) {
			rel, _ := filepath.Rel(tree, path)
			t.Errorf("the archive holds a copy of the machine key at %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeKeyTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBuildArchive_DefaultLayoutKeepsTheMachineKeyOut(t *testing.T) {
	configRoot := filepath.Join(t.TempDir(), "etc", "hoserva")
	writeKeyTestFile(t, filepath.Join(configRoot, "secret.key"), machineKeyBytes)
	writeKeyTestFile(t, filepath.Join(configRoot, "other.conf"), "kept\n")

	paths := DefaultPaths(t.TempDir(), configRoot)
	staging := buildKeyTestArchive(t, paths)

	requireNoMachineKey(t, staging)
	if got, err := os.ReadFile(filepath.Join(staging, "generated", "other.conf")); err != nil || string(got) != "kept\n" {
		t.Fatalf("generated/other.conf = %q, %v; the exclusion must leave the other files", got, err)
	}
}

func TestBuildArchive_ExcludesTheMachineKeyAtItsConfiguredPath(t *testing.T) {
	configRoot := filepath.Join(t.TempDir(), "hoserva")
	keyPath := filepath.Join(configRoot, "keys", "host-seal.bin")
	writeKeyTestFile(t, keyPath, machineKeyBytes)
	writeKeyTestFile(t, filepath.Join(configRoot, "keys", "seal.conf"), "kept\n")

	staging := buildKeyTestArchive(t, Paths{ConfigRoot: configRoot, MachineKeyPath: keyPath})

	requireNoMachineKey(t, staging)
	if _, err := os.Stat(filepath.Join(staging, "generated", "keys", "seal.conf")); err != nil {
		t.Fatalf("a file next to the key was dropped too: %v", err)
	}
}

func TestBuildArchive_ExcludesTheMachineKeyUnderAnotherSpelling(t *testing.T) {
	configRoot := filepath.Join(t.TempDir(), "hoserva")
	keyPath := filepath.Join(configRoot, "host-seal.bin")
	writeKeyTestFile(t, keyPath, machineKeyBytes)
	if err := os.Symlink(keyPath, filepath.Join(configRoot, "alias.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(keyPath, filepath.Join(configRoot, "hardlink.bin")); err != nil {
		t.Fatal(err)
	}

	for name, configured := range map[string]string{
		"with dot segments": filepath.Join(configRoot, "sub", "..", "host-seal.bin"),
		"through a symlink": filepath.Join(configRoot, "alias.bin"),
	} {
		t.Run(name, func(t *testing.T) {
			staging := buildKeyTestArchive(t, Paths{ConfigRoot: configRoot, MachineKeyPath: configured})
			requireNoMachineKey(t, staging)
		})
	}
}

func TestBuildArchive_ExcludesTheMachineKeyFromEveryCopiedTree(t *testing.T) {
	root := t.TempDir()
	keyPath := filepath.Join(root, "seal.custom.conf")
	writeKeyTestFile(t, keyPath, machineKeyBytes)
	templates := filepath.Join(root, "templates")
	writeKeyTestFile(t, filepath.Join(templates, "app.yml"), "kept\n")
	if err := os.Link(keyPath, filepath.Join(templates, "copy.yml")); err != nil {
		t.Fatal(err)
	}
	content := filepath.Join(root, "content")
	if err := os.MkdirAll(content, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(keyPath, filepath.Join(content, "snapraid.content")); err != nil {
		t.Fatal(err)
	}

	staging := buildKeyTestArchive(t, Paths{ConfigRoot: root, TemplatesDir: templates, SnapraidContentPath: content, MachineKeyPath: keyPath})

	requireNoMachineKey(t, staging)
	if _, err := os.Stat(filepath.Join(staging, "templates", "app.yml")); err != nil {
		t.Fatalf("templates/app.yml missing: %v", err)
	}
}

func TestBuildArchive_ExcludesTheMachineKeyByNameWhenNoPathIsConfigured(t *testing.T) {
	configRoot := filepath.Join(t.TempDir(), "hoserva")
	writeKeyTestFile(t, filepath.Join(configRoot, "secret.key"), machineKeyBytes)
	writeKeyTestFile(t, filepath.Join(configRoot, "nested", "secret.key"), machineKeyBytes)

	staging := buildKeyTestArchive(t, Paths{ConfigRoot: configRoot})

	requireNoMachineKey(t, staging)
}

func TestBuildArchive_AMachineKeyThatCannotBeExaminedFailsTheBuild(t *testing.T) {
	configRoot := filepath.Join(t.TempDir(), "hoserva")
	writeKeyTestFile(t, filepath.Join(configRoot, "a.conf"), "x\n")
	notADir := filepath.Join(t.TempDir(), "file")
	writeKeyTestFile(t, notADir, "x")
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
	staging := filepath.Join(dir, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := Paths{ConfigRoot: configRoot, MachineKeyPath: filepath.Join(notADir, "secret.key")}
	if _, err := BuildArchive(ctx, db, paths, nil, nil, "host", "test", time.Now(), staging); err == nil {
		t.Fatal("BuildArchive succeeded although the machine key path cannot be examined")
	}
}

func TestDefaultPaths_NamesTheMachineKeyInTheConfigRoot(t *testing.T) {
	if got := DefaultPaths("", "").MachineKeyPath; got != "/etc/hoserva/secret.key" {
		t.Fatalf("default MachineKeyPath = %q", got)
	}
	if got := DefaultPaths("", "/x/hoserva").MachineKeyPath; got != "/x/hoserva/secret.key" {
		t.Fatalf("MachineKeyPath under a custom root = %q", got)
	}
}
