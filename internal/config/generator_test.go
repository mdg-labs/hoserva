package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testFile() File {
	return File{
		Path:    "snapraid.conf",
		Command: "sync",
		Body:    []byte("parity /mnt/parity/snapraid.parity\n"),
	}
}

func TestWriteRendersHeaderAndBody(t *testing.T) {
	g := NewGenerator(t.TempDir())
	now := time.Date(2026, 9, 14, 10, 33, 12, 0, time.UTC)

	if err := g.Write(context.Background(), testFile(), 1, now); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(g.Root, "snapraid.conf"))
	if err != nil {
		t.Fatalf("reading generated file: %v", err)
	}

	want := Header("sync", 1, now) + "parity /mnt/parity/snapraid.parity\n"
	if string(got) != want {
		t.Fatalf("generated file mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteGoesUnderConfigurableRoot(t *testing.T) {
	root := t.TempDir()
	g := NewGenerator(root)

	if err := g.Write(context.Background(), testFile(), 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "snapraid.conf")); err != nil {
		t.Fatalf("Write did not write under Root: %v", err)
	}
}

func TestWriteIsAtomicAndLeavesNoTempFile(t *testing.T) {
	g := NewGenerator(t.TempDir())

	if err := g.Write(context.Background(), testFile(), 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries, err := os.ReadDir(g.Root)
	if err != nil {
		t.Fatalf("reading Root: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("Write left a temp file behind: %s", e.Name())
		}
	}
}

func TestWriteCreatesMissingDirectories(t *testing.T) {
	g := NewGenerator(t.TempDir())
	file := testFile()
	file.Path = "samba/smb.conf"

	if err := g.Write(context.Background(), file, 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(g.Root, "samba", "smb.conf")); err != nil {
		t.Fatalf("Write did not create the parent directory: %v", err)
	}
}

func TestWriteRejectsPathsThatEscapeRoot(t *testing.T) {
	g := NewGenerator(t.TempDir())

	for _, path := range []string{
		"../outside.conf",
		"samba/../../outside.conf",
		"/etc/snapraid.conf",
	} {
		file := testFile()
		file.Path = path
		if err := g.Write(context.Background(), file, 1, time.Now()); err == nil {
			t.Fatalf("Write(%q) did not error", path)
		}
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(g.Root), "outside.conf")); err == nil {
		t.Fatal("Write escaped Root despite returning an error")
	}
}

func TestWriteRejectsTheReservedManifestDirectory(t *testing.T) {
	g := NewGenerator(t.TempDir())

	for _, path := range []string{".hoserva", ".hoserva/manifest.json"} {
		file := testFile()
		file.Path = path
		if err := g.Write(context.Background(), file, 1, time.Now()); err == nil {
			t.Fatalf("Write(%q) did not error", path)
		}
	}
}

func TestCheckAndDiffAndKeepUnmanagedRejectEscapingPaths(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()

	if _, err := g.Check(ctx, "../outside.conf"); err == nil {
		t.Fatal("Check on an escaping path did not error")
	}
	if _, err := g.Diff(ctx, File{Path: "../outside.conf"}, 1, time.Now()); err == nil {
		t.Fatal("Diff on an escaping path did not error")
	}
	if err := g.KeepUnmanaged(ctx, "../outside.conf"); err == nil {
		t.Fatal("KeepUnmanaged on an escaping path did not error")
	}
}

func TestWriteRefusesAnExistingHostFileUntilImported(t *testing.T) {
	root := t.TempDir()
	g := NewGenerator(root)
	ctx := context.Background()
	full := filepath.Join(root, "samba", "smb.conf")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "[media]\npath = /srv/media\n"
	if err := os.WriteFile(full, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	file := File{Path: PathSamba, Command: "share create", Body: []byte("[global]\n")}
	if err := g.Write(ctx, file, 1, time.Now()); !errors.Is(err, ErrExistingHostFile) {
		t.Fatalf("Write on an existing host file = %v, want ErrExistingHostFile", err)
	}
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("Write changed the existing host file:\n%s", got)
	}

	if err := g.RecordImported(ctx, PathSamba); err != nil {
		t.Fatalf("RecordImported: %v", err)
	}
	if err := g.Write(ctx, file, 1, time.Now()); err != nil {
		t.Fatalf("Write after import: %v", err)
	}
}

func TestWriteRefusesAnUnmanagedFile(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	file := testFile()

	if err := g.Write(ctx, file, 1, time.Now()); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := g.KeepUnmanaged(ctx, file.Path); err != nil {
		t.Fatalf("KeepUnmanaged: %v", err)
	}

	if err := g.Write(ctx, file, 2, time.Now()); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("Write on an unmanaged file = %v, want ErrUnmanaged", err)
	}
}
