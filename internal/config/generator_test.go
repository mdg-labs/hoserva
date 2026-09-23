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

// TestWriteDefaultsToWorldReadableMode proves a File with Mode left zero
// lands at defaultFileMode — the behaviour every generated file had before
// #260 added File.Mode, preserved for a file with nothing secret in it.
func TestWriteDefaultsToWorldReadableMode(t *testing.T) {
	g := NewGenerator(t.TempDir())

	if err := g.Write(context.Background(), testFile(), 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(filepath.Join(g.Root, "snapraid.conf"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != defaultFileMode {
		t.Fatalf("mode = %o, want %o", got, defaultFileMode)
	}
}

// TestWriteHonorsExplicitMode proves a File that sets Mode overrides
// defaultFileMode — the mechanism nut.go's WriteUPS uses to land
// upsmon.conf and upsd.users at secretFileMode instead (#260).
func TestWriteHonorsExplicitMode(t *testing.T) {
	g := NewGenerator(t.TempDir())
	file := testFile()
	file.Mode = secretFileMode

	if err := g.Write(context.Background(), file, 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(filepath.Join(g.Root, "snapraid.conf"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != secretFileMode {
		t.Fatalf("mode = %o, want %o", got, secretFileMode)
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

func TestAtomicWriteExclusiveRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "smb.conf")
	original := "[media]\npath = /srv/media\n"
	if err := os.WriteFile(dest, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(dest, []byte("[global]\n"), 0o644, -1, true); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive atomicWrite = %v, want os.ErrExist", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("exclusive write changed the destination:\n%s", got)
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

func TestCanWriteMatchesWriteRefusal(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	t.Run("unmanaged", func(t *testing.T) {
		g := NewGenerator(t.TempDir())
		file := testFile()
		if err := g.Write(ctx, file, 1, now); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := g.KeepUnmanaged(ctx, file.Path); err != nil {
			t.Fatalf("KeepUnmanaged: %v", err)
		}
		before, err := os.ReadFile(filepath.Join(g.Root, file.Path))
		if err != nil {
			t.Fatal(err)
		}
		if err := g.CanWrite(ctx, file.Path); !errors.Is(err, ErrUnmanaged) {
			t.Fatalf("CanWrite = %v, want ErrUnmanaged", err)
		}
		after, err := os.ReadFile(filepath.Join(g.Root, file.Path))
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatal("CanWrite must not change the file")
		}
	})

	t.Run("existing host file", func(t *testing.T) {
		root := t.TempDir()
		g := NewGenerator(root)
		full := filepath.Join(root, PathNFS)
		original := "/export/media *(ro)\n"
		if err := os.WriteFile(full, []byte(original), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := g.CanWrite(ctx, PathNFS); !errors.Is(err, ErrExistingHostFile) {
			t.Fatalf("CanWrite = %v, want ErrExistingHostFile", err)
		}
		got, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != original {
			t.Fatalf("CanWrite changed the host file:\n%s", got)
		}
	})

	t.Run("missing path is writable", func(t *testing.T) {
		g := NewGenerator(t.TempDir())
		if err := g.CanWrite(ctx, PathSamba); err != nil {
			t.Fatalf("CanWrite on missing path = %v", err)
		}
		if _, err := os.Stat(filepath.Join(g.Root, PathSamba)); !os.IsNotExist(err) {
			t.Fatal("CanWrite must not create the file")
		}
	})
}
