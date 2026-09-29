package backup

import (
	"archive/tar"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	_ "modernc.org/sqlite"
)

func buildArchiveWithSecrets(t *testing.T, dir string) string {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatalf("opening live database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("creating table: %v", err)
	}

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

	staging := filepath.Join(dir, "staging")
	if _, err := BuildArchive(ctx, db, Paths{}, src, FakeSecretCipher{}, "host", "test", time.Now(), staging); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}

	archivePath := filepath.Join(dir, "archive.tar.zst")
	if err := packArchive(staging, archivePath); err != nil {
		t.Fatalf("packArchive: %v", err)
	}
	return archivePath
}

// TestVerifyArchive_RequiresPassphraseWhenSecretsAgePresent is the
// existing nightly-backup behaviour VerifyArchiveForImport must not
// change: the full VerifyArchive keeps refusing an archive with
// secrets.age and no passphrase.
func TestVerifyArchive_RequiresPassphraseWhenSecretsAgePresent(t *testing.T) {
	archivePath := buildArchiveWithSecrets(t, t.TempDir())
	if err := VerifyArchive(archivePath, ""); err == nil {
		t.Fatal("VerifyArchive(archive with secrets.age, no passphrase) = nil, want an error")
	}
	if err := VerifyArchive(archivePath, "test-pass"); err != nil {
		t.Fatalf("VerifyArchive(archive with secrets.age, correct passphrase): %v", err)
	}
}

// TestVerifyArchiveForImport_SkipsSecretsAgeWithoutPassphrase is #269's
// own requirement: config import (doc 10 §1) restores the database only
// — #62 restores secrets — so an archive with secrets.age must still
// pass checksum and integrity verification with no passphrase at all.
func TestVerifyArchiveForImport_SkipsSecretsAgeWithoutPassphrase(t *testing.T) {
	archivePath := buildArchiveWithSecrets(t, t.TempDir())
	if err := VerifyArchiveForImport(archivePath); err != nil {
		t.Fatalf("VerifyArchiveForImport(archive with secrets.age, no passphrase): %v", err)
	}
}

// TestVerifyArchiveForImport_StillCatchesChecksumMismatch confirms the
// import-only verifier still runs the same structural checks as
// VerifyArchive — only the secrets.age passphrase requirement is skipped.
func TestVerifyArchiveForImport_StillCatchesChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatalf("opening live database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("creating table: %v", err)
	}
	staging := filepath.Join(dir, "staging")
	if _, err := BuildArchive(ctx, db, Paths{}, nil, nil, "host", "test", time.Now(), staging); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	// Corrupt the staged state.db after the manifest checksum was
	// computed over its original bytes.
	if err := (func() error {
		f, err := sql.Open("sqlite", filepath.Join(staging, "state.db"))
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = f.Exec("INSERT INTO t (id) VALUES (999)")
		return err
	})(); err != nil {
		t.Fatalf("corrupting staged state.db: %v", err)
	}
	archivePath := filepath.Join(dir, "archive.tar.zst")
	if err := packArchive(staging, archivePath); err != nil {
		t.Fatalf("packArchive: %v", err)
	}

	if err := VerifyArchiveForImport(archivePath); err == nil {
		t.Fatal("VerifyArchiveForImport(archive with tampered state.db) = nil, want a checksum-mismatch error")
	}
}

// buildStaging builds a real archive's tree with BuildArchive, with the
// generated, custom, template and stack files the layout has, and returns
// the staging directory (not yet packed).
func buildStaging(t *testing.T, withSecrets, withIdentity bool) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatalf("opening live database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("creating table: %v", err)
	}

	root := filepath.Join(dir, "etc")
	for rel, body := range map[string]string{
		"snapraid.conf":        "content\n",
		"smb.custom.conf":      "[custom]\n",
		"stacks/media/compose": "services: {}\n",
		"templates/app.json":   "{}\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}
	paths := Paths{
		ConfigRoot:   root,
		StacksDir:    filepath.Join(root, "stacks"),
		TemplatesDir: filepath.Join(root, "templates"),
	}

	src := &FakeSecretSource{Passphrase: "test-pass", HasPass: true}
	if withSecrets {
		src.Secrets = []DatabaseSecret{{Table: "notify_channels", Column: "secret", RowID: "ch1", Ciphertext: []byte{0x5a}}}
	}
	var opts []ArchiveOption
	if withIdentity {
		recipient, err := LoadOrGenerateRecipient(context.Background(), FakeSecretCipher{}, &FakeRecipientStore{}, nil)
		if err != nil {
			t.Fatalf("LoadOrGenerateRecipient: %v", err)
		}
		opts = append(opts, WithRecipient(recipient))
	}
	staging := filepath.Join(dir, "staging")
	if _, err := BuildArchive(context.Background(), db, paths, src, FakeSecretCipher{}, "host", "test", time.Now(), staging, opts...); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	return staging
}

type rawEntry struct {
	hdr  tar.Header
	body string
}

// packWithEntries packs staging like packArchive and then appends entries
// exactly as given, so a test can write what packArchive never would.
func packWithEntries(t *testing.T, staging string, extra ...rawEntry) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "archive.tar.zst")
	out, err := os.Create(dest)
	if err != nil {
		t.Fatalf("creating archive: %v", err)
	}
	zw, err := zstd.NewWriter(out)
	if err != nil {
		t.Fatalf("creating zstd writer: %v", err)
	}
	tw := tar.NewWriter(zw)
	if err := addDirToTar(tw, staging, ""); err != nil {
		t.Fatalf("packing staging: %v", err)
	}
	for _, e := range extra {
		hdr := e.hdr
		hdr.Size = int64(len(e.body))
		if hdr.Typeflag != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("writing header %q: %v", hdr.Name, err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("writing %q: %v", hdr.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zstd: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("closing archive: %v", err)
	}
	return dest
}

func requireVerifyRefuses(t *testing.T, archivePath, wantSubstring string) {
	t.Helper()
	for name, verify := range map[string]func(string) error{
		"VerifyArchive":          func(p string) error { return VerifyArchive(p, "test-pass") },
		"VerifyArchiveForImport": VerifyArchiveForImport,
	} {
		err := verify(archivePath)
		if err == nil {
			t.Fatalf("%s accepted the archive, want a refusal mentioning %q", name, wantSubstring)
		}
		if !strings.Contains(err.Error(), wantSubstring) {
			t.Fatalf("%s error = %v, want it to mention %q", name, err, wantSubstring)
		}
	}
}

func TestVerifyArchive_AcceptsFreshlyBuiltArchive(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		withSecrets, withIdentity bool
	}{
		{"neither", false, false},
		{"secrets.age only", true, false},
		{"identity.age only", false, true},
		{"both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staging := buildStaging(t, tc.withSecrets, tc.withIdentity)
			if _, err := os.Stat(filepath.Join(staging, "secrets.age")); (err == nil) != tc.withSecrets {
				t.Fatalf("secrets.age present = %v, want %v", err == nil, tc.withSecrets)
			}
			if _, err := os.Stat(filepath.Join(staging, "identity.age")); (err == nil) != tc.withIdentity {
				t.Fatalf("identity.age present = %v, want %v", err == nil, tc.withIdentity)
			}
			archivePath := filepath.Join(t.TempDir(), "archive.tar.zst")
			if err := packArchive(staging, archivePath); err != nil {
				t.Fatalf("packArchive: %v", err)
			}
			if err := VerifyArchive(archivePath, "test-pass"); err != nil {
				t.Fatalf("VerifyArchive: %v", err)
			}
			if err := VerifyArchiveForImport(archivePath); err != nil {
				t.Fatalf("VerifyArchiveForImport: %v", err)
			}
		})
	}
}

// A regular file the manifest does not list is never checksummed, so it
// used to pass verification untouched.
func TestVerifyArchive_RefusesFileTheManifestDoesNotList(t *testing.T) {
	for _, rel := range []string{
		"generated/extra.conf",
		"stacks/media/extra.yml",
		"templates/extra.json",
		"custom/extra.custom.conf",
		"extra.txt",
	} {
		t.Run(rel, func(t *testing.T) {
			staging := buildStaging(t, true, true)
			p := filepath.Join(staging, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatalf("creating %s: %v", filepath.Dir(p), err)
			}
			if err := os.WriteFile(p, []byte("not in the manifest"), 0o600); err != nil {
				t.Fatalf("writing %s: %v", p, err)
			}
			archivePath := filepath.Join(t.TempDir(), "archive.tar.zst")
			if err := packArchive(staging, archivePath); err != nil {
				t.Fatalf("packArchive: %v", err)
			}
			requireVerifyRefuses(t, archivePath, "not listed in the manifest")
		})
	}
}

func TestVerifyArchive_RefusesStateDBTheManifestDoesNotList(t *testing.T) {
	staging := buildStaging(t, false, false)
	m, err := readManifest(filepath.Join(staging, "manifest.json"))
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	delete(m.Checksums, "state.db")
	if err := writeManifest(filepath.Join(staging, "manifest.json"), m); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "archive.tar.zst")
	if err := packArchive(staging, archivePath); err != nil {
		t.Fatalf("packArchive: %v", err)
	}
	requireVerifyRefuses(t, archivePath, "state.db")
}

func TestVerifyArchive_RefusesMissingListedFile(t *testing.T) {
	staging := buildStaging(t, true, true)
	if err := os.Remove(filepath.Join(staging, "templates", "app.json")); err != nil {
		t.Fatalf("removing a listed file: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "archive.tar.zst")
	if err := packArchive(staging, archivePath); err != nil {
		t.Fatalf("packArchive: %v", err)
	}
	requireVerifyRefuses(t, archivePath, "templates/app.json")
}

func TestVerifyArchive_RefusesManifestNamingAPathOutsideTheArchive(t *testing.T) {
	staging := buildStaging(t, false, false)
	m, err := readManifest(filepath.Join(staging, "manifest.json"))
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	m.Checksums["../outside"] = strings.Repeat("0", 64)
	if err := writeManifest(filepath.Join(staging, "manifest.json"), m); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "archive.tar.zst")
	if err := packArchive(staging, archivePath); err != nil {
		t.Fatalf("packArchive: %v", err)
	}
	requireVerifyRefuses(t, archivePath, "../outside")
}

func TestVerifyArchive_RefusesEntryThatIsNotARegularFileOrDirectory(t *testing.T) {
	for name, hdr := range map[string]tar.Header{
		"symlink":   {Name: "generated/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777},
		"hardlink":  {Name: "generated/hard", Typeflag: tar.TypeLink, Linkname: "state.db", Mode: 0o600},
		"fifo":      {Name: "generated/fifo", Typeflag: tar.TypeFifo, Mode: 0o600},
		"char dev":  {Name: "generated/dev", Typeflag: tar.TypeChar, Mode: 0o600},
		"block dev": {Name: "generated/blk", Typeflag: tar.TypeBlock, Mode: 0o600},
	} {
		t.Run(name, func(t *testing.T) {
			archivePath := packWithEntries(t, buildStaging(t, true, true), rawEntry{hdr: hdr})
			requireVerifyRefuses(t, archivePath, "not a regular file or directory")
		})
	}
}

func TestVerifyArchive_RefusesDuplicateEntryName(t *testing.T) {
	staging := buildStaging(t, true, true)
	original, err := os.ReadFile(filepath.Join(staging, "templates", "app.json"))
	if err != nil {
		t.Fatalf("reading templates/app.json: %v", err)
	}
	t.Run("same content", func(t *testing.T) {
		archivePath := packWithEntries(t, staging, rawEntry{
			hdr:  tar.Header{Name: "templates/app.json", Typeflag: tar.TypeReg, Mode: 0o600},
			body: string(original),
		})
		requireVerifyRefuses(t, archivePath, "duplicate")
	})
	t.Run("replaced content", func(t *testing.T) {
		archivePath := packWithEntries(t, staging, rawEntry{
			hdr:  tar.Header{Name: "./templates/app.json", Typeflag: tar.TypeReg, Mode: 0o600},
			body: "something else",
		})
		requireVerifyRefuses(t, archivePath, "duplicate")
	})
	t.Run("directory listed twice", func(t *testing.T) {
		archivePath := packWithEntries(t, staging, rawEntry{
			hdr: tar.Header{Name: "templates/", Typeflag: tar.TypeDir, Mode: 0o700},
		})
		requireVerifyRefuses(t, archivePath, "duplicate")
	})
}
