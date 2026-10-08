package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
// defaultFileMode — the mechanism a caller with a credential to protect
// uses to land a file at a restricted mode instead (#260).
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

func assertManifestOwnerOnly(t *testing.T, root string) {
	t.Helper()
	dirInfo, err := os.Stat(filepath.Join(root, ".hoserva"))
	if err != nil {
		t.Fatalf("stat .hoserva: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf(".hoserva mode = %o, want 700", got)
	}
	fileInfo, err := os.Stat(filepath.Join(root, ".hoserva", "manifest.json"))
	if err != nil {
		t.Fatalf("stat manifest.json: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("manifest.json mode = %o, want 600", got)
	}
}

// seedWorldReadableManifest leaves the manifest and its directory the way an
// earlier release wrote them (0644 in 0755), empty.
func seedWorldReadableManifest(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, ".hoserva")
	manifest := filepath.Join(dir, "manifest.json")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Mkdir and WriteFile are subject to the umask; chmod is not.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifest, 0o644); err != nil {
		t.Fatal(err)
	}
}

// withUmask runs the test under umask 0, so a mode that holds only because
// the process umask masked it fails.
func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

// TestWriteUPSKeepsTheManifestOwnerOnly proves the manifest, which holds a
// digest of each root:nut 0640 NUT file, is readable by root alone however
// permissive the process umask is.
func TestWriteUPSKeepsTheManifestOwnerOnly(t *testing.T) {
	withUmask(t, 0)
	g := newUPSGenerator(t, t.TempDir())

	writeUPS(t, g, loadUPSState(t, "usb"), 1, time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC))

	assertManifestOwnerOnly(t, g.Root)
}

func TestWriteTightensAWorldReadableManifest(t *testing.T) {
	withUmask(t, 0)
	g := NewGenerator(t.TempDir())
	seedWorldReadableManifest(t, g.Root)

	if err := g.Write(context.Background(), testFile(), 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	assertManifestOwnerOnly(t, g.Root)
}

func TestDriftChecksTightenAWorldReadableManifest(t *testing.T) {
	ctx := context.Background()
	checks := map[string]func(g *Generator) error{
		"Check": func(g *Generator) error {
			_, err := g.Check(ctx, "snapraid.conf")
			return err
		},
		"CheckAll": func(g *Generator) error {
			_, err := g.CheckAll(ctx)
			return err
		},
		"CanWrite": func(g *Generator) error {
			return g.CanWrite(ctx, "snapraid.conf")
		},
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			g := NewGenerator(t.TempDir())
			seedWorldReadableManifest(t, g.Root)

			if err := check(g); err != nil {
				t.Fatalf("%s: %v", name, err)
			}

			assertManifestOwnerOnly(t, g.Root)
		})
	}
}

func TestLoadingAMissingManifestCreatesNothing(t *testing.T) {
	g := NewGenerator(t.TempDir())

	if _, err := g.Check(context.Background(), "snapraid.conf"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	if _, err := os.Stat(filepath.Join(g.Root, ".hoserva")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a drift check created .hoserva: %v", err)
	}
}

func TestWriteRefusesAManifestDirectoryThatIsAFile(t *testing.T) {
	g := NewGenerator(t.TempDir())
	if err := os.WriteFile(filepath.Join(g.Root, ".hoserva"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := g.Write(context.Background(), testFile(), 1, time.Now()); err == nil {
		t.Fatal("Write succeeded with a file where the manifest directory belongs")
	}
}
