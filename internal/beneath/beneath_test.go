package beneath

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func mkdirs(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestComponents(t *testing.T) {
	if got, err := Components("a/b/c.txt"); err != nil || len(got) != 3 || got[2] != "c.txt" {
		t.Fatalf("Components = %v, %v", got, err)
	}
	for _, bad := range []string{"", "/a", "a//b", "a/../b", "..", ".", "a/./b", "a/"} {
		if _, err := Components(bad); err == nil {
			t.Errorf("Components(%q) succeeded, want an error", bad)
		}
	}
}

func TestOpen_RefusesASymlinkWhateverItPointsAt(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "file")); err != nil {
		t.Fatal(err)
	}
	rfd, err := OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(rfd) }()

	if _, err := Open(rfd, "dir", unix.O_RDONLY|unix.O_DIRECTORY); !errors.Is(err, ErrSymlink) {
		t.Errorf("Open(symlink to a directory, O_DIRECTORY) = %v, want ErrSymlink", err)
	}
	if _, err := Open(rfd, "dir", unix.O_RDONLY); !errors.Is(err, ErrSymlink) {
		t.Errorf("Open(symlink to a directory) = %v, want ErrSymlink", err)
	}
	if _, err := Open(rfd, "file", unix.O_RDONLY); !errors.Is(err, ErrSymlink) {
		t.Errorf("Open(symlink to a file) = %v, want ErrSymlink", err)
	}
	if _, err := Open(rfd, "missing", unix.O_RDONLY); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(missing) = %v, want ErrNotExist", err)
	}
	for _, bad := range []string{"a/b", "..", ".", ""} {
		if _, err := Open(rfd, bad, unix.O_RDONLY); err == nil {
			t.Errorf("Open(%q) succeeded, want an error", bad)
		}
	}
}

func TestWalker_DescendsAndReusesTheHeldChain(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a/b/c", "a/x")
	w, err := NewWalker(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	var opened []string
	record := func(_, _ int, rel string) error { opened = append(opened, rel); return nil }
	for _, rel := range []string{"a/b/c", "a/b", "a/x", "a/b/c"} {
		if _, err := w.Dir(rel, record); err != nil {
			t.Fatalf("Dir(%s): %v", rel, err)
		}
	}
	want := []string{"a", "a/b", "a/b/c", "a/x", "a/b", "a/b/c"}
	if len(opened) != len(want) {
		t.Fatalf("opened %v, want %v", opened, want)
	}
	for i := range want {
		if opened[i] != want[i] {
			t.Fatalf("opened %v, want %v", opened, want)
		}
	}
	if fd, err := w.Dir("", nil); err != nil || fd != w.Root() {
		t.Fatalf(`Dir("") = %d, %v, want the root`, fd, err)
	}
}

func TestWalker_StopsAtASymlinkedAncestor(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mkdirs(t, outside, "sub")
	mkdirs(t, root, "a")
	if err := os.Symlink(outside, filepath.Join(root, "a", "link")); err != nil {
		t.Fatal(err)
	}
	w, err := NewWalker(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := w.Dir("a/link/sub", nil); !errors.Is(err, ErrSymlink) {
		t.Fatalf("Dir through a symlink = %v, want ErrSymlink", err)
	}
	if _, err := w.Dir("a/nowhere/sub", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Dir through a missing directory = %v, want ErrNotExist", err)
	}
}

func TestWalker_AHeldDirectoryStaysTheOneThatWasOpened(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mkdirs(t, root, "a")
	w, err := NewWalker(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	fd, err := w.Dir("a", nil)
	if err != nil {
		t.Fatal(err)
	}
	var want unix.Stat_t
	if err := unix.Fstat(fd, &want); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	fd, err = w.Dir("a", nil)
	if err != nil {
		t.Fatalf("Dir on a held directory: %v", err)
	}
	var got unix.Stat_t
	if err := unix.Fstat(fd, &got); err != nil {
		t.Fatal(err)
	}
	if got.Ino != want.Ino || got.Dev != want.Dev {
		t.Fatal("a held directory descriptor now names another directory")
	}
}

func TestWalker_OpenedErrorDropsTheDirectoryAndIsReportedAgain(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a/b")
	w, err := NewWalker(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	boom := errors.New("boom")
	calls := 0
	failA := func(_, _ int, rel string) error {
		if rel == "a" {
			calls++
			return boom
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		if _, err := w.Dir("a/b", failA); !errors.Is(err, boom) {
			t.Fatalf("Dir = %v, want boom", err)
		}
	}
	if calls != 2 {
		t.Fatalf("opened called %d times for a, want it again on the second walk", calls)
	}
}

func TestOpenResolvedDir_RefusesALinkInAnyComponentTheLastIncluded(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	mkdirs(t, base, "real/inner")
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}

	fd, err := OpenResolvedDir(filepath.Join(base, "real", "inner"))
	if err != nil {
		t.Fatalf("OpenResolvedDir(real directory) = %v", err)
	}
	var want, got unix.Stat_t
	if err := unix.Stat(filepath.Join(base, "real", "inner"), &want); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fstat(fd, &got); err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(fd)
	if got.Ino != want.Ino || got.Dev != want.Dev {
		t.Fatalf("the descriptor is not the directory asked for: %+v vs %+v", got, want)
	}

	for _, p := range []string{
		filepath.Join(base, "link"),
		filepath.Join(base, "alias"),
		filepath.Join(base, "alias", "inner"),
	} {
		if fd, err := OpenResolvedDir(p); !errors.Is(err, ErrSymlink) {
			if err == nil {
				_ = unix.Close(fd)
			}
			t.Errorf("OpenResolvedDir(%s) = %v, want ErrSymlink", p, err)
		}
	}
	if _, err := OpenResolvedDir(filepath.Join(base, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenResolvedDir(missing) = %v, want ErrNotExist", err)
	}
	if _, err := OpenResolvedDir("relative/dir"); err == nil {
		t.Error("OpenResolvedDir(relative path) succeeded")
	}
}

func TestNewWalkerAt_WalksBeneathTheDescriptorItWasGivenAndOwnsIt(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a/b")
	fd, err := OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	w := NewWalkerAt(fd)
	if w.Root() != fd {
		t.Fatalf("Root() = %d, want %d", w.Root(), fd)
	}
	dfd, err := w.Dir("a/b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkdirat(dfd, "made", 0o700); err != nil {
		t.Fatal(err)
	}
	if !exists(t, filepath.Join(root, "a", "b", "made")) {
		t.Fatal("the directory was not made beneath the walker's root")
	}
	w.Close()
	if err := unix.Fstat(fd, &unix.Stat_t{}); !errors.Is(err, unix.EBADF) {
		t.Fatalf("Fstat after Close = %v, want EBADF: the walker owns its root", err)
	}
}

func TestReadNames_ListsADirectoryAndLeavesItsDescriptorOpen(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "d", "e")
	fd := openTestRoot(t, root)
	names, err := ReadNames(fd)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("ReadNames = %v, want d and e", names)
	}
	if err := unix.Fstat(fd, &unix.Stat_t{}); err != nil {
		t.Fatalf("the descriptor was closed by ReadNames: %v", err)
	}
}
