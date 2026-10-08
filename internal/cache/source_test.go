package cache

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// copyMoveFile is how the tests copy a source named by a path: the source
// root is src with as many trailing components removed as dst has beneath
// dstRoot, the correspondence copyEntry's callers keep (the source's path
// beneath its root ends the way the target's does), and the metadata is the
// one the test chose. The production code never derives a root; it is given
// the one it trusts.
func copyMoveFile(src, dst, dstRoot string, srcInfo os.FileInfo, cfg Config, deps Deps) error {
	rel, err := relBeneath(dstRoot, filepath.Dir(dst))
	if err != nil {
		return err
	}
	root, depth := filepath.Dir(src), 0
	if rel != "" {
		depth = len(strings.Split(rel, string(filepath.Separator)))
	}
	for i := 0; i < depth; i++ {
		root = filepath.Dir(root)
	}
	entry, err := openSource(root, strings.TrimPrefix(src, root+string(filepath.Separator)))
	if err != nil {
		return err
	}
	defer entry.Close()
	entry.info = srcInfo
	return copyEntry(entry, dst, dstRoot, cfg, deps)
}

func stampOfPath(t *testing.T, path string) sourceStamp {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return stampOf(info)
}

// srcDirInfos is the metadata of dir and its n-1 ancestors, outermost first.
func srcDirInfos(t *testing.T, dir string, n int) []os.FileInfo {
	t.Helper()
	infos := make([]os.FileInfo, n)
	for i := n - 1; i >= 0; i, dir = i-1, filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		infos[i] = info
	}
	return infos
}

// The copy reads its source relative to the directory that holds it,
// opened from the source root without following a link (#776). These tests
// swap a directory above the source for a symlink to a directory holding a
// file of the same name, at the moments a share user could, and check what
// reaches the target: only ever the real file's content, and a real source
// is deleted only when that content was what was copied.

const swapReal = "real content"

// swapBackOnce undoes swapOnce for dir: the symlink goes, the directory
// moved aside returns. It does nothing unless dir is a symlink now.
func swapBackOnce(t *testing.T, dir string) func() {
	t.Helper()
	done := false
	return func() {
		if done {
			return
		}
		done = true
		if info, err := os.Lstat(dir); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is not swapped for a symlink: %v", dir, err)
			return
		}
		if err := os.Remove(dir); err != nil {
			t.Errorf("removing the symlink %s: %v", dir, err)
			return
		}
		if err := os.Rename(dir+".moved", dir); err != nil {
			t.Errorf("moving %s back: %v", dir, err)
		}
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("reading %s: %v", path, err)
		return
	}
	if string(got) != want {
		t.Errorf("%s holds %q, want %q", path, got, want)
	}
}

// assertSwapMidCopy checks the outcome of a swap made after the entry was
// chosen: the target holds the real file's content, never the outside
// file's, and the source is kept when the swap is still in place at the
// delete and removed, its content copied, once it was undone.
func assertSwapMidCopy(t *testing.T, report Report, back bool, rel, target, outside, reports string) {
	t.Helper()
	assertFileContent(t, target, swapReal)
	assertFileContent(t, filepath.Join(outside, filepath.Base(rel)), "outside data")
	e, ok := resultFor(report, rel)
	if !ok {
		t.Fatalf("no entry for %s", rel)
	}
	if back {
		if e.Result != ResultMoved {
			t.Errorf("entry = %+v, want moved", e)
		}
		if _, err := os.Lstat(filepath.Join(reports, filepath.Base(rel))); !os.IsNotExist(err) {
			t.Errorf("the source is still there after its content was copied: %v", err)
		}
		return
	}
	assertSwapRefused(t, report, rel, outside, reports+".moved")
}

func TestRun_SourceDirectorySwappedMidCopyCopiesTheRealFile(t *testing.T) {
	for _, back := range []bool{false, true} {
		t.Run(map[bool]string{false: "stays swapped", true: "swapped back before the delete"}[back], func(t *testing.T) {
			s := newShare(t, "docs")
			reports := filepath.Join(s.CachePath, "reports")
			mustWrite(t, filepath.Join(reports, "Q3.txt"), swapReal)
			if err := os.MkdirAll(filepath.Join(s.ArrayPath, "reports"), 0o755); err != nil {
				t.Fatal(err)
			}
			outside := outsideWith(t, "Q3.txt")
			swap, swapBack := swapOnce(t, reports, outside), swapBackOnce(t, reports)

			deps := testDeps(NewFakeOpenChecker())
			deps.UUID = func() string {
				swap()
				return "test-uuid"
			}
			if back {
				deps.FsyncDir = func(dirfd int) error {
					swapBack()
					return fsyncDir(dirfd)
				}
			}
			report, err := Run(context.Background(), []Share{s}, Config{SkipGracePeriod: true, VerifyChecksum: true}, deps, RunHooks{}, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			assertSwapMidCopy(t, report, back, "reports/Q3.txt", filepath.Join(s.ArrayPath, "reports", "Q3.txt"), outside, reports)
		})
	}
}

func TestRelocateToCache_SourceDirectorySwappedMidCopyCopiesTheRealFile(t *testing.T) {
	for _, back := range []bool{false, true} {
		t.Run(map[bool]string{false: "stays swapped", true: "swapped back before the delete"}[back], func(t *testing.T) {
			s := relocateShare(t, "docs", 1)
			reports := filepath.Join(s.Branches[0], "reports")
			mustWrite(t, filepath.Join(reports, "Q3.txt"), swapReal)
			if err := os.MkdirAll(filepath.Join(s.CachePath, "reports"), 0o755); err != nil {
				t.Fatal(err)
			}
			outside := outsideWith(t, "Q3.txt")
			swap, swapBack := swapOnce(t, reports, outside), swapBackOnce(t, reports)

			deps := testDeps(NewFakeOpenChecker())
			deps.UUID = func() string {
				swap()
				return "test-uuid"
			}
			deps.Sync = func(context.Context, []parity.ManifestEntry) error {
				if back {
					swapBack()
				}
				return nil
			}
			report, err := RelocateToCache(context.Background(), s, Config{VerifyChecksum: true}, deps, RunHooks{}, nil)
			if err != nil {
				t.Fatalf("RelocateToCache: %v", err)
			}
			assertSwapMidCopy(t, report, back, "reports/Q3.txt", filepath.Join(s.CachePath, "reports", "Q3.txt"), outside, reports)
		})
	}
}

func rebalanceSwapFixture(t *testing.T) (plan RebalancePlan, source, target string) {
	t.Helper()
	base := t.TempDir()
	source = filepath.Join(base, "disk1", "movies")
	target = filepath.Join(base, "disk2", "movies")
	mustWrite(t, filepath.Join(source, "reports", "Q3.txt"), swapReal)
	if err := os.MkdirAll(filepath.Join(target, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan = RebalancePlan{Moves: []RebalanceMove{{Share: "movies", RelPath: "reports/Q3.txt", SourceBranch: source, TargetBranch: target, Size: int64(len(swapReal))}}}
	return plan, source, target
}

func TestRunRebalance_SourceDirectorySwappedMidCopyCopiesTheRealFile(t *testing.T) {
	for _, back := range []bool{false, true} {
		t.Run(map[bool]string{false: "stays swapped", true: "swapped back before the delete"}[back], func(t *testing.T) {
			plan, source, target := rebalanceSwapFixture(t)
			reports := filepath.Join(source, "reports")
			outside := outsideWith(t, "Q3.txt")
			swap, swapBack := swapOnce(t, reports, outside), swapBackOnce(t, reports)

			deps := rebalanceTestDeps(NewFakeOpenChecker())
			deps.UUID = func() string {
				swap()
				return "test-uuid"
			}
			deps.Sync = func(context.Context, []parity.ManifestEntry) error {
				if back {
					swapBack()
				}
				return nil
			}
			report, err := RunRebalance(context.Background(), plan, Config{VerifyChecksum: true}, deps, RunHooks{}, nil)
			if err != nil {
				t.Fatalf("RunRebalance: %v", err)
			}
			assertSwapMidCopy(t, report, back, "reports/Q3.txt", filepath.Join(target, "reports", "Q3.txt"), outside, reports)
		})
	}
}

func TestRelocateToCache_SourceDirectorySwappedForASymlinkBeforeTheFileIsChosenCopiesNothing(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	reports := filepath.Join(s.Branches[0], "reports")
	mustWrite(t, filepath.Join(reports, "Q3.txt"), swapReal)
	outside := outsideWith(t, "Q3.txt")

	deps := testDeps(NewFakeOpenChecker())
	deps.Open = swapSnapshotter{FakeOpenChecker: NewFakeOpenChecker(), swap: swapOnce(t, reports, outside)}
	deps.Sync = func(context.Context, []parity.ManifestEntry) error { return nil }
	report, err := RelocateToCache(context.Background(), s, Config{VerifyChecksum: true}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	assertSwapRefused(t, report, "reports/Q3.txt", outside, reports+".moved")
	if _, err := os.Lstat(filepath.Join(s.CachePath, "reports", "Q3.txt")); err == nil {
		t.Error("the file outside the tree was copied onto the cache")
	}
}

func TestRunRebalance_SourceDirectorySwappedForASymlinkBeforeTheFileIsChosenCopiesNothing(t *testing.T) {
	plan, source, target := rebalanceSwapFixture(t)
	reports := filepath.Join(source, "reports")
	outside := outsideWith(t, "Q3.txt")

	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.Open = swapSnapshotter{FakeOpenChecker: NewFakeOpenChecker(), swap: swapOnce(t, reports, outside)}
	deps.Sync = func(context.Context, []parity.ManifestEntry) error { return nil }
	report, err := RunRebalance(context.Background(), plan, Config{VerifyChecksum: true}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	assertSwapRefused(t, report, "reports/Q3.txt", outside, reports+".moved")
	if _, err := os.Lstat(filepath.Join(target, "reports", "Q3.txt")); err == nil {
		t.Error("the file outside the tree was copied onto the target disk")
	}
}

func TestPlanEvacuation_RunViaRunRebalance_SourceDirectorySwappedMidCopyCopiesTheRealFile(t *testing.T) {
	base := t.TempDir()
	disk1, disk2 := filepath.Join(base, "disk1"), filepath.Join(base, "disk2")
	s := evacuateShare(t, "movies", []string{disk1, disk2})
	reports := filepath.Join(s.Branches[0], "reports")
	mustWrite(t, filepath.Join(reports, "Q3.txt"), swapReal)
	if err := os.MkdirAll(filepath.Join(s.Branches[1], "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := outsideWith(t, "Q3.txt")
	swap := swapOnce(t, reports, outside)

	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.Usage = fakeUsage(map[string]DiskUsage{s.Branches[1]: {TotalBytes: 1000, FreeBytes: 900}})
	plan, err := PlanEvacuation(context.Background(), disk1, []Share{s}, deps)
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	deps.UUID = func() string {
		swap()
		return "test-uuid"
	}
	deps.Sync = func(context.Context, []parity.ManifestEntry) error { return nil }
	report, err := RunRebalance(context.Background(), plan, Config{VerifyChecksum: true}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	assertSwapMidCopy(t, report, false, "reports/Q3.txt", filepath.Join(s.Branches[1], "reports", "Q3.txt"), outside, reports)
}

// A source rewritten after it was copied is not the file whose content was
// verified onto the target: it is kept, not deleted (the identity the copy
// read is the identity the delete checks).

func assertSourceChangedKept(t *testing.T, report Report, rel, src, want string) {
	t.Helper()
	e, ok := resultFor(report, rel)
	if !ok || e.Result != ResultFailed {
		t.Errorf("entry for %s = %+v, want failed", rel, e)
	}
	assertFileContent(t, src, want)
}

func rewriteLater(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func TestRun_ASourceRewrittenAfterTheCopyIsKept(t *testing.T) {
	s := newShare(t, "docs")
	src := filepath.Join(s.CachePath, "Q3.txt")
	mustWrite(t, src, swapReal)

	deps := testDeps(NewFakeOpenChecker())
	deps.FsyncDir = func(dirfd int) error {
		rewriteLater(t, src, "a newer version of the numbers")
		return fsyncDir(dirfd)
	}
	report, err := Run(context.Background(), []Share{s}, Config{SkipGracePeriod: true}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertSourceChangedKept(t, report, "Q3.txt", src, "a newer version of the numbers")
}

func TestRelocateToCache_ASourceRewrittenAfterTheCopyIsKept(t *testing.T) {
	s := relocateShare(t, "docs", 1)
	src := filepath.Join(s.Branches[0], "Q3.txt")
	mustWrite(t, src, swapReal)

	deps := testDeps(NewFakeOpenChecker())
	deps.Sync = func(context.Context, []parity.ManifestEntry) error {
		rewriteLater(t, src, "a newer version of the numbers")
		return nil
	}
	report, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	assertSourceChangedKept(t, report, "Q3.txt", src, "a newer version of the numbers")
}

func TestRunRebalance_ASourceRewrittenAfterTheCopyIsKept(t *testing.T) {
	plan, source, _ := rebalanceSwapFixture(t)
	src := filepath.Join(source, "reports", "Q3.txt")

	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.Sync = func(context.Context, []parity.ManifestEntry) error {
		rewriteLater(t, src, "a newer version of the numbers")
		return nil
	}
	report, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	assertSourceChangedKept(t, report, "reports/Q3.txt", src, "a newer version of the numbers")
}

func TestOpenSource_RefusesASymlinkAmongTheDirectories(t *testing.T) {
	root := t.TempDir()
	outside := outsideWith(t, "f.txt")
	mustWrite(t, filepath.Join(root, "real", "f.txt"), swapReal)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"link/f.txt", "alias/f.txt"} {
		if src, err := openSource(root, rel); err == nil || !strings.Contains(err.Error(), "is a symbolic link") {
			if src != nil {
				src.Close()
			}
			t.Errorf("openSource(%s) = %v, want an error naming the symlink", rel, err)
		}
	}
}

// Once the source is open, reading it never looks at the path again: the
// content, the link target and the checksum come from the directory that was
// opened, wherever the name leads now.
func TestSourceEntry_ReadsTheOpenedDirectoryAfterItIsSwapped(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "reports", "Q3.txt"), swapReal)
	if err := os.Symlink("Q3.txt", filepath.Join(root, "reports", "latest")); err != nil {
		t.Fatal(err)
	}
	outside := outsideWith(t, "Q3.txt")
	if err := os.Symlink("outside-target", filepath.Join(outside, "latest")); err != nil {
		t.Fatal(err)
	}
	file, err := openSource(root, "reports/Q3.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	link, err := openSource(root, "reports/latest")
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	swapOnce(t, filepath.Join(root, "reports"), outside)()

	want, err := hashFile(filepath.Join(root, "reports.moved", "Q3.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := file.hash(); err != nil || got != want {
		t.Errorf("hash = %q, %v, want %q: the real file's", got, err, want)
	}
	f, err := file.openFile()
	if err != nil {
		t.Fatalf("openFile: %v", err)
	}
	defer func() { _ = f.Close() }()
	if got, err := io.ReadAll(f); err != nil || string(got) != swapReal {
		t.Errorf("the opened file holds %q, %v, want the real file's content", got, err)
	}
	if got, err := link.readlink(); err != nil || got != "Q3.txt" {
		t.Errorf("readlink = %q, %v, want the real link's target", got, err)
	}
}

// A name that now leads to another file than the one the entry's metadata
// was read from is refused, so a read never mixes two files. The originals are
// removed, not renamed aside, so the replacements usually get their inode
// numbers on ext4 and XFS: only the change time tells them apart. The pause
// keeps the replacements out of the clock tick the originals were created in.
func TestSourceEntry_RefusesAFileReplacedAfterItWasOpened(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "f.txt"), swapReal)
	if err := os.Symlink("a", filepath.Join(root, "ln")); err != nil {
		t.Fatal(err)
	}
	file, err := openSource(root, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	link, err := openSource(root, "ln")
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	time.Sleep(50 * time.Millisecond)
	for _, name := range []string{"f.txt", "ln"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(root, "f.txt"), "another file")
	if err := os.Symlink("b", filepath.Join(root, "ln")); err != nil {
		t.Fatal(err)
	}

	if _, err := file.hash(); err == nil {
		t.Error("hash of a replaced file succeeded")
	}
	if err := file.copyXattrsTo(filepath.Join(root, "f.txt")); err == nil {
		t.Error("copying the extended attributes of a replaced file succeeded")
	}
	if got, err := link.readlink(); err == nil {
		t.Errorf("readlink of a replaced link = %q, want an error", got)
	}
}

type stampInfo struct {
	os.FileInfo
	size  int64
	mtime time.Time
}

func (s stampInfo) Size() int64        { return s.size }
func (s stampInfo) ModTime() time.Time { return s.mtime }

func TestSourceStampMatches(t *testing.T) {
	whole := time.Unix(1_700_000_000, 0)
	fractional := time.Unix(1_700_000_000, 123_456_789)
	tests := []struct {
		name  string
		stamp sourceStamp
		info  stampInfo
		want  bool
	}{
		{"identical", sourceStamp{10, fractional}, stampInfo{size: 10, mtime: fractional}, true},
		{"whole second touched within the second", sourceStamp{10, whole}, stampInfo{size: 10, mtime: whole.Add(987 * time.Millisecond)}, true},
		{"whole second, another second", sourceStamp{10, whole}, stampInfo{size: 10, mtime: whole.Add(time.Second)}, false},
		{"whole second, an earlier second", sourceStamp{10, whole}, stampInfo{size: 10, mtime: whole.Add(-time.Nanosecond)}, false},
		{"whole second, another size", sourceStamp{10, whole}, stampInfo{size: 11, mtime: whole.Add(time.Millisecond)}, false},
		{"fractional, another sub-second part", sourceStamp{10, fractional}, stampInfo{size: 10, mtime: fractional.Add(time.Nanosecond)}, false},
		{"fractional, the whole second", sourceStamp{10, fractional}, stampInfo{size: 10, mtime: whole}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.stamp.matches(tt.info); got != tt.want {
				t.Errorf("matches = %v, want %v", got, tt.want)
			}
		})
	}
}
