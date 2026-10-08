//go:build linux

package share

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestListConfined_ReadsHoldingDiskXattrThroughTheDescriptor(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sub", "film.mkv"), "movie")
	if err := os.Mkdir(filepath.Join(root, "sub", "shows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filepath.Join(root, "sub", "film.mkv"), MergerFSBasepath, []byte("/mnt/disk1"), 0); err != nil {
		t.Skipf("the temp directory's filesystem takes no user xattrs: %v", err)
	}

	entries, err := (OSFS{}).ListConfined(root, "sub")
	if err != nil {
		t.Fatalf("ListConfined: %v", err)
	}
	want := []BrowseEntry{
		{Name: "film.mkv", SizeBytes: 5, Disk: "/mnt/disk1"},
		{Name: "shows", Directory: true},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v, want %+v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("entries[%d] = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

func TestListConfined_ReportsASymlinkEntryAsItself(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	entries, err := (OSFS{}).ListConfined(root, "")
	if err != nil {
		t.Fatalf("ListConfined: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "link" || entries[0].Directory {
		t.Fatalf("entries = %+v, want link listed as a non-directory", entries)
	}
}

func TestListConfined_RefusesASymlinkInTheDirectoryPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "b", "secret"), "no")
	if err := os.Symlink(outside, filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}

	if _, err := (OSFS{}).ListConfined(root, "a/b"); !errors.Is(err, ErrPathEscapes) {
		t.Fatalf("ListConfined through a symlinked component = %v, want ErrPathEscapes", err)
	}
}
