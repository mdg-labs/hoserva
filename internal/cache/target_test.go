package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/beneath"
	"github.com/mdg-labs/hoserva/internal/parity"
)

// swapForSymlink returns a UUID hook that, the first time the copy asks for
// its temp name — after the target directory has been resolved and before
// the temp file is created — moves dir aside and puts a symlink to
// outside in its place, the way a share user can at any moment (#656).
func swapForSymlink(t *testing.T, dir, outside string) func() string {
	t.Helper()
	swapped := false
	return func() string {
		if !swapped {
			swapped = true
			if err := os.Rename(dir, dir+".moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, dir); err != nil {
				t.Fatal(err)
			}
		}
		return "test-uuid"
	}
}

func raceFixture(t *testing.T) (src, dstRoot, outside string) {
	t.Helper()
	base := t.TempDir()
	srcRoot := filepath.Join(base, "src")
	dstRoot, outside = filepath.Join(base, "dst"), filepath.Join(base, "outside")
	mustWrite(t, filepath.Join(srcRoot, "a/f.txt"), "data")
	if err := os.Symlink("f.txt", filepath.Join(srcRoot, "a/link")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(dstRoot, "a"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return srcRoot, dstRoot, outside
}

func assertNothingEscaped(t *testing.T, src, outside, moved string) {
	t.Helper()
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("%d entries were written outside the target tree", len(entries))
	}
	if _, err := os.Lstat(src); err != nil {
		t.Errorf("the source was touched: %v", err)
	}
	if got := tempNamed(t, moved); len(got) != 0 {
		t.Errorf("temp files left behind: %v", got)
	}
	if _, err := os.Lstat(filepath.Join(moved, filepath.Base(src))); err == nil {
		t.Errorf("a copy was published in the directory that was swapped away")
	}
}

func TestCopyMoveFile_ADirectorySwappedForASymlinkMidCopyFailsAndWritesNothingOutside(t *testing.T) {
	for _, name := range []string{"f.txt", "link"} {
		t.Run(name, func(t *testing.T) {
			srcRoot, dstRoot, outside := raceFixture(t)
			src := filepath.Join(srcRoot, "a", name)
			info, err := os.Lstat(src)
			if err != nil {
				t.Fatal(err)
			}
			deps := testDeps(NewFakeOpenChecker())
			deps.UUID = swapForSymlink(t, filepath.Join(dstRoot, "a"), outside)

			err = copyMoveFile(src, filepath.Join(dstRoot, "a", name), dstRoot, info, Config{VerifyChecksum: true}, deps.withDefaults())
			if !errors.Is(err, beneath.ErrSymlink) {
				t.Fatalf("copyMoveFile = %v, want beneath.ErrSymlink", err)
			}
			assertNothingEscaped(t, src, outside, filepath.Join(dstRoot, "a.moved"))
		})
	}
}

func TestRun_ADirectorySwappedForASymlinkMidCopyIsAFailedEntryAndKeepsTheSource(t *testing.T) {
	s := newShare(t, "docs")
	mustWrite(t, filepath.Join(s.CachePath, "a/f.txt"), "data")
	outside := filepath.Join(filepath.Dir(s.ArrayPath), "outside")
	if err := os.MkdirAll(filepath.Join(s.ArrayPath, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	deps := testDeps(NewFakeOpenChecker())
	deps.UUID = swapForSymlink(t, filepath.Join(s.ArrayPath, "a"), outside)

	report, err := Run(context.Background(), []Share{s}, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	e, _ := resultFor(report, "a/f.txt")
	if e.Result != ResultFailed || e.Err == "" {
		t.Fatalf("entry = %+v, want failed with the symlink named", e)
	}
	assertNothingEscaped(t, filepath.Join(s.CachePath, "a/f.txt"), outside, filepath.Join(s.ArrayPath, "a.moved"))
}

func TestRelocateToCache_AShareDirectorySwappedForASymlinkMidCopyKeepsTheSource(t *testing.T) {
	s := newShare(t, "docs")
	s.Branches = []string{filepath.Join(filepath.Dir(s.CachePath), "disk1", "docs")}
	mustWrite(t, filepath.Join(s.Branches[0], "f.txt"), "data")
	outside := filepath.Join(filepath.Dir(s.CachePath), "outside")
	for _, d := range []string{s.CachePath, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	deps := testDeps(NewFakeOpenChecker())
	deps.UUID = swapForSymlink(t, s.CachePath, outside)
	deps.Sync = func(context.Context, []parity.ManifestEntry) error { return nil }

	report, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	e, _ := resultFor(report, "f.txt")
	if e.Result != ResultFailed || e.Err == "" {
		t.Fatalf("entry = %+v, want failed with the symlink named", e)
	}
	assertNothingEscaped(t, filepath.Join(s.Branches[0], "f.txt"), outside, s.CachePath+".moved")
}

func TestRunRebalance_AShareDirectorySwappedForASymlinkOnTheTargetDiskKeepsTheSource(t *testing.T) {
	base := t.TempDir()
	source, target := filepath.Join(base, "disk1", "movies"), filepath.Join(base, "disk2", "movies")
	outside := filepath.Join(base, "outside")
	plan := rebalancePlanMoves(t, "movies", source, target, 1)
	for _, d := range []string{target, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.UUID = swapForSymlink(t, target, outside)
	deps.Sync = func(context.Context, []parity.ManifestEntry) error { return nil }

	report, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	mv := plan.Moves[0]
	if e, _ := resultFor(report, mv.RelPath); e.Result != ResultFailed || e.Err == "" {
		t.Fatalf("entry = %+v, want failed with the symlink named", e)
	}
	assertNothingEscaped(t, filepath.Join(mv.SourceBranch, mv.RelPath), outside, target+".moved")
}

// A path the kernel refuses for a symlink (ELOOP) — a branch's nosymfollow
// bind under the mover's mergerfs mount, or a source swapped for a symlink
// after it was chosen — fails the copy with the same named error the walk
// gives, never a bare errno.
func TestCopyMoveFile_ASymlinkTheKernelRefusesIsNamed(t *testing.T) {
	srcRoot, dstRoot, outside := raceFixture(t)
	src := filepath.Join(srcRoot, "a", "f.txt")
	info, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), src); err != nil {
		t.Fatal(err)
	}

	err = copyMoveFile(src, filepath.Join(dstRoot, "a", "f.txt"), dstRoot, info, Config{}, testDeps(NewFakeOpenChecker()).withDefaults())
	if !errors.Is(err, beneath.ErrSymlink) {
		t.Fatalf("copyMoveFile = %v, want beneath.ErrSymlink", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dstRoot, "a")); len(entries) != 0 {
		t.Fatalf("%d entries were left in the target directory", len(entries))
	}
}
