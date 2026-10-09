package cache

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
	"github.com/mdg-labs/hoserva/internal/parity"
)

// dirModes are chosen to differ from what os.MkdirAll(…, 0o755) leaves
// behind, one with the setgid bit a share directory carries (Q26).
const (
	topDirMode   = os.ModeDir | 0o750
	innerDirMode = os.ModeDir | os.ModeSetgid | 0o770
)

func buildDirMetaTree(t *testing.T, root string) {
	t.Helper()
	mustWrite(t, filepath.Join(root, "docs/Reports/Q3.txt"), "quarterly numbers")
	for rel, mode := range map[string]os.FileMode{"docs": topDirMode, "docs/Reports": innerDirMode} {
		if err := os.Chmod(filepath.Join(root, rel), mode); err != nil {
			t.Fatalf("chmod %s: %v", rel, err)
		}
	}
}

func assertDirLike(t *testing.T, root string) {
	t.Helper()
	self := os.Getuid()
	for rel, want := range map[string]os.FileMode{"docs": topDirMode, "docs/Reports": innerDirMode} {
		info, err := os.Lstat(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("lstat %s: %v", rel, err)
		}
		if info.Mode() != want {
			t.Errorf("%s: mode %v, want %v", rel, info.Mode(), want)
		}
		if st := info.Sys().(*syscall.Stat_t); int(st.Uid) != self {
			t.Errorf("%s: owner %d, want %d", rel, st.Uid, self)
		}
	}
}

func TestCopyMoveFile_CreatesTargetDirectoriesLikeTheSource(t *testing.T) {
	base := t.TempDir()
	srcRoot, dstRoot := filepath.Join(base, "src"), filepath.Join(base, "dst")
	buildDirMetaTree(t, srcRoot)
	src := filepath.Join(srcRoot, "docs/Reports/Q3.txt")
	info, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyMoveFile(src, filepath.Join(dstRoot, "docs/Reports/Q3.txt"), dstRoot, info, Config{}, testDeps(NewFakeOpenChecker()).withDefaults()); err != nil {
		t.Fatalf("copyMoveFile: %v", err)
	}
	assertDirLike(t, dstRoot)
}

func TestCopyMoveFile_CreatesTargetDirectoriesLikeTheSourceForANode(t *testing.T) {
	base := t.TempDir()
	srcRoot, dstRoot := filepath.Join(base, "src"), filepath.Join(base, "dst")
	buildDirMetaTree(t, srcRoot)
	if err := os.Symlink("Q3.txt", filepath.Join(srcRoot, "docs/Reports/latest")); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcRoot, "docs/Reports/latest")
	info, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyMoveFile(src, filepath.Join(dstRoot, "docs/Reports/latest"), dstRoot, info, Config{}, testDeps(NewFakeOpenChecker()).withDefaults()); err != nil {
		t.Fatalf("copyMoveFile: %v", err)
	}
	assertDirLike(t, dstRoot)
}

func TestCopyMoveFile_LeavesAnExistingTargetDirectoryAlone(t *testing.T) {
	base := t.TempDir()
	srcRoot, dstRoot := filepath.Join(base, "src"), filepath.Join(base, "dst")
	buildDirMetaTree(t, srcRoot)
	src := filepath.Join(srcRoot, "docs/Reports/Q3.txt")
	info, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dstRoot, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyMoveFile(src, filepath.Join(dstRoot, "docs/Reports/Q3.txt"), dstRoot, info, Config{}, testDeps(NewFakeOpenChecker()).withDefaults()); err != nil {
		t.Fatalf("copyMoveFile: %v", err)
	}
	if got, _ := os.Lstat(filepath.Join(dstRoot, "docs")); got.Mode() != os.ModeDir|0o755 {
		t.Errorf("docs already existed on the target and is now %v: only directories the copy creates take the source's mode", got.Mode())
	}
	if got, _ := os.Lstat(filepath.Join(dstRoot, "docs/Reports")); got.Mode() != innerDirMode {
		t.Errorf("docs/Reports: mode %v, want %v", got.Mode(), innerDirMode)
	}
}

func TestRelocateToArray_CreatesArrayDirectoriesLikeTheCacheOnes(t *testing.T) {
	s := newShare(t, "documents")
	buildDirMetaTree(t, s.CachePath)

	if _, err := RelocateToArray(context.Background(), s, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil); err != nil {
		t.Fatalf("RelocateToArray: %v", err)
	}
	assertDirLike(t, s.ArrayPath)
}

func TestRelocateToCache_CreatesCacheDirectoriesLikeTheArrayOnes(t *testing.T) {
	s := newShare(t, "documents")
	s.Branches = []string{filepath.Join(filepath.Dir(s.CachePath), "disk1", "documents")}
	buildDirMetaTree(t, s.Branches[0])
	deps := testDeps(NewFakeOpenChecker())
	deps.Sync = func(context.Context, []parity.ManifestEntry) error { return nil }

	if _, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, nil); err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	assertDirLike(t, s.CachePath)
}

func tempNamed(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), tempSuffix) {
			found = append(found, e.Name())
		}
	}
	return found
}

func TestMkdirAllLike_LeavesNoTempDirectoryBehind(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src", "a", "b")
	if err := os.MkdirAll(src, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o750); err != nil {
		t.Fatal(err)
	}
	dstRoot := filepath.Join(base, "dst")
	if err := os.Mkdir(dstRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fd, err := mkdirAllLike(srcDirInfos(t, src, 2), dstRoot, filepath.Join(dstRoot, "a", "b"), testDeps(NewFakeOpenChecker()).withDefaults())
	if err != nil {
		t.Fatalf("mkdirAllLike: %v", err)
	}
	_ = unix.Close(fd)
	for _, d := range []string{dstRoot, filepath.Join(dstRoot, "a")} {
		if got := tempNamed(t, d); len(got) != 0 {
			t.Fatalf("temp directories left in %s after a successful create: %v", d, got)
		}
	}
	if info, err := os.Lstat(filepath.Join(dstRoot, "a", "b")); err != nil || info.Mode() != os.ModeDir|0o750 {
		t.Fatalf("a/b = %v, %v, want a 0750 directory", info, err)
	}
}

func TestMkdirAllLike_RefusesADestinationOutsideTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := mkdirAllLike(nil, root, filepath.Join(base, "elsewhere"), testDeps(NewFakeOpenChecker()).withDefaults()); err == nil {
		t.Fatal("mkdirAllLike created a directory outside its root")
	}
	if _, err := os.Lstat(filepath.Join(base, "elsewhere")); err == nil {
		t.Fatal("a directory outside the root was created")
	}
}

// A directory that cannot be given the source's owner never has its real
// name, so a retry cannot take it for finished and leave it root-owned
// (doc 09 §2). Root can chown to anyone, so this runs unprivileged.
func TestMkdirAllLike_AFailedChownLeavesNothingUnderTheRealName(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can give a directory to anyone")
	}
	base := t.TempDir()
	dstRoot := filepath.Join(base, "dst")
	if err := os.Mkdir(dstRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// "/" is root's: the daemon's user cannot hand a directory to it.
	if _, err := mkdirAllLike(srcDirInfos(t, "/", 1), dstRoot, filepath.Join(dstRoot, "new"), testDeps(NewFakeOpenChecker()).withDefaults()); err == nil {
		t.Fatal("mkdirAllLike succeeded although the owner could not be set")
	}
	if _, err := os.Lstat(filepath.Join(dstRoot, "new")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("new directory after a failed create: %v, want it absent", err)
	}
	if got := tempNamed(t, dstRoot); len(got) != 0 {
		t.Fatalf("temp directories left after a failed create: %v", got)
	}
}

// A parent directory of the new one that is a symlink is refused wherever
// it points, and nothing is created behind it: the directory the copy
// would have made, and the owner it would have given it, are never put
// outside the target tree (#626).
func TestCopyMoveFile_RefusesASymlinkedParentOfANewDirectory(t *testing.T) {
	base := t.TempDir()
	srcRoot, dstRoot, outside := filepath.Join(base, "src"), filepath.Join(base, "dst"), filepath.Join(base, "outside")
	mustWrite(t, filepath.Join(srcRoot, "a/b/f.txt"), "data")
	for _, d := range []string{dstRoot, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dstRoot, "a")); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcRoot, "a/b/f.txt")
	info, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}

	err = copyMoveFile(src, filepath.Join(dstRoot, "a/b/f.txt"), dstRoot, info, Config{}, testDeps(NewFakeOpenChecker()).withDefaults())
	if !errors.Is(err, beneath.ErrSymlink) {
		t.Fatalf("copyMoveFile through a symlinked parent = %v, want beneath.ErrSymlink", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("the copy created %d entries outside the target tree", len(entries))
	}
}

func TestCopyMoveFile_RefusesASymlinkedParentForANode(t *testing.T) {
	base := t.TempDir()
	srcRoot, dstRoot, outside := filepath.Join(base, "src"), filepath.Join(base, "dst"), filepath.Join(base, "outside")
	mustWrite(t, filepath.Join(srcRoot, "a/b/f.txt"), "data")
	if err := os.Symlink("f.txt", filepath.Join(srcRoot, "a/b/link")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dstRoot, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dstRoot, "a")); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcRoot, "a/b/link")
	info, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}

	err = copyMoveFile(src, filepath.Join(dstRoot, "a/b/link"), dstRoot, info, Config{}, testDeps(NewFakeOpenChecker()).withDefaults())
	if !errors.Is(err, beneath.ErrSymlink) {
		t.Fatalf("copyMoveFile of a node through a symlinked parent = %v, want beneath.ErrSymlink", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("the copy created %d entries outside the target tree", len(entries))
	}
}

func TestSweepStrayTemps_RemovesAnEmptyTempDirectoryAndKeepsANonEmptyOne(t *testing.T) {
	root := t.TempDir()
	stray := filepath.Join(root, "docs"+tempSuffix+"abc")
	full := filepath.Join(root, "other"+tempSuffix+"def")
	for _, d := range []string{stray, full} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(full, "keep.txt"), "data")

	if err := sweepStrayTemps(root); err != nil {
		t.Fatalf("sweepStrayTemps: %v", err)
	}
	if _, err := os.Lstat(stray); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the empty temp directory survived: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(full, "keep.txt")); err != nil {
		t.Errorf("a file in a non-empty temp-named directory was removed: %v", err)
	}
}
