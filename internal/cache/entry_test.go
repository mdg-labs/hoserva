package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// entryTree is the mixed tree every relocation path below must carry
// across: a regular file, relative, absolute and dangling symlinks, a
// symlink to a directory, a FIFO, a socket, a sparse file and a hard-linked
// pair. Device nodes need CAP_MKNOD and are covered by the lab tests.
type entryTree struct {
	root        string
	externalDir string
	sparseSize  int64
}

const entryMTimeAgo = time.Hour

func (e entryTree) path(rel string) string { return filepath.Join(e.root, rel) }

func buildEntryTree(t *testing.T, root string) entryTree {
	t.Helper()
	tree := entryTree{root: root, externalDir: t.TempDir(), sparseSize: 8 << 20}
	old := time.Now().Add(-entryMTimeAgo).Truncate(time.Second)

	mustWrite(t, tree.path("plain.txt"), "plain bytes")
	mustWrite(t, tree.externalDir+"/outside.txt", "outside bytes")

	links := map[string]string{
		"app/rel-link":      "../plain.txt",
		"app/abs-link":      filepath.Join(tree.externalDir, "outside.txt"),
		"app/dangling":      "does/not/exist",
		"app/dir-link":      tree.externalDir,
		"app/nested/second": "../../plain.txt",
	}
	for rel, target := range links {
		if err := os.MkdirAll(filepath.Dir(tree.path(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, tree.path(rel)); err != nil {
			t.Fatalf("symlink %s: %v", rel, err)
		}
	}

	if err := unix.Mkfifo(tree.path("app/queue"), 0o640); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	tree.makeSocket(t, "app/runtime.sock")

	sparse, err := os.OpenFile(tree.path("vm.img"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := sparse.Truncate(tree.sparseSize); err != nil {
		t.Fatal(err)
	}
	if _, err := sparse.WriteAt([]byte("head"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := sparse.WriteAt([]byte("middle"), tree.sparseSize/2); err != nil {
		t.Fatal(err)
	}
	if err := sparse.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := sparse.Close(); err != nil {
		t.Fatal(err)
	}
	if blocks(t, tree.path("vm.img")) >= tree.sparseSize/2 {
		t.Skip("the temp filesystem does not keep holes")
	}

	mustWrite(t, tree.path("data/original"), "one inode, two names")
	if err := os.Link(tree.path("data/original"), tree.path("data/alias")); err != nil {
		t.Fatalf("link: %v", err)
	}

	for _, rel := range tree.entries(t) {
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, tree.path(rel), []unix.Timespec{unix.NsecToTimespec(old.UnixNano()), unix.NsecToTimespec(old.UnixNano())}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			t.Fatalf("utimes %s: %v", rel, err)
		}
	}
	return tree
}

func (e entryTree) makeSocket(t *testing.T, rel string) {
	t.Helper()
	dir, err := os.Open(filepath.Dir(e.path(rel)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(rel)), Net: "unix"})
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	l.SetUnlinkOnClose(false)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

// entries lists every non-directory entry under the tree, relative.
func (e entryTree) entries(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(e.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(e.root, p)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func blocks(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

// movedEntries is every entry buildEntryTree makes except the socket.
var movedEntries = []string{
	"plain.txt", "app/rel-link", "app/abs-link", "app/dangling", "app/dir-link",
	"app/nested/second", "app/queue", "vm.img", "data/original", "data/alias",
}

// assertSameEntry checks that dst carries src's type, link target, mode,
// owner, timestamps, and allocated size to within one block.
func assertSameEntry(t *testing.T, srcInfo os.FileInfo, wantTarget, dstPath string) {
	t.Helper()
	dstInfo, err := os.Lstat(dstPath)
	if err != nil {
		t.Fatalf("%s missing on the target: %v", dstPath, err)
	}
	if srcInfo.Mode().Type() != dstInfo.Mode().Type() {
		t.Fatalf("%s: type %v, want %v", dstPath, dstInfo.Mode().Type(), srcInfo.Mode().Type())
	}
	if srcInfo.Mode().Type() == fs.ModeSymlink {
		got, err := os.Readlink(dstPath)
		if err != nil || got != wantTarget {
			t.Fatalf("%s: link target %q (%v), want %q", dstPath, got, err, wantTarget)
		}
	} else if srcInfo.Mode().Perm() != dstInfo.Mode().Perm() {
		t.Fatalf("%s: mode %v, want %v", dstPath, dstInfo.Mode().Perm(), srcInfo.Mode().Perm())
	}
	if !srcInfo.ModTime().Equal(dstInfo.ModTime()) {
		t.Fatalf("%s: mtime %v, want %v", dstPath, dstInfo.ModTime(), srcInfo.ModTime())
	}
	s, d := srcInfo.Sys().(*syscall.Stat_t), dstInfo.Sys().(*syscall.Stat_t)
	if s.Uid != d.Uid || s.Gid != d.Gid {
		t.Fatalf("%s: owner %d:%d, want %d:%d", dstPath, d.Uid, d.Gid, s.Uid, s.Gid)
	}
	if srcInfo.Mode().IsRegular() {
		if d.Blocks*512 > s.Blocks*512+int64(s.Blksize) {
			t.Fatalf("%s: %d bytes allocated, source holds %d (holes were written out)", dstPath, d.Blocks*512, s.Blocks*512)
		}
	}
}

type entrySnapshot map[string]snapshotItem

type snapshotItem struct {
	info   os.FileInfo
	target string
	rdev   uint64
}

func snapshotEntries(t *testing.T, tree entryTree, rels []string) entrySnapshot {
	t.Helper()
	snap := entrySnapshot{}
	for _, rel := range rels {
		info, err := os.Lstat(tree.path(rel))
		if err != nil {
			t.Fatal(err)
		}
		item := snapshotItem{info: info, rdev: uint64(info.Sys().(*syscall.Stat_t).Rdev)}
		if info.Mode()&fs.ModeSymlink != 0 {
			item.target = readLinkOrFail(t, tree.path(rel))
		}
		snap[rel] = item
	}
	return snap
}

func (s entrySnapshot) assertOn(t *testing.T, dstRoot string) {
	t.Helper()
	for rel, item := range s {
		assertSameEntry(t, item.info, item.target, filepath.Join(dstRoot, rel))
	}
}

func readLinkOrFail(t *testing.T, p string) string {
	t.Helper()
	target, err := os.Readlink(p)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func assertGoneOrLeft(t *testing.T, root string, rels []string, wantLeft bool) {
	t.Helper()
	for _, rel := range rels {
		_, err := os.Lstat(filepath.Join(root, rel))
		switch {
		case wantLeft && err != nil:
			t.Errorf("%s should still exist: %v", filepath.Join(root, rel), err)
		case !wantLeft && err == nil:
			t.Errorf("%s should have been deleted", filepath.Join(root, rel))
		case !wantLeft && !errors.Is(err, fs.ErrNotExist):
			t.Errorf("%s: %v", filepath.Join(root, rel), err)
		}
	}
}

func resultFor(report Report, rel string) (Entry, bool) {
	for _, e := range report.Entries {
		if e.Path == rel {
			return e, true
		}
	}
	return Entry{}, false
}

func TestRun_MovesEveryEntryType(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(fmt.Sprintf("checksum=%v", verify), func(t *testing.T) {
			s := newShare(t, "appdata")
			tree := buildEntryTree(t, s.CachePath)
			snap := snapshotEntries(t, tree, movedEntries)
			extBefore := readLinkOrFail(t, tree.path("app/dir-link"))

			report, err := Run(context.Background(), []Share{s}, Config{VerifyChecksum: verify}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			snap.assertOn(t, s.ArrayPath)
			if got := readLinkOrFail(t, filepath.Join(s.ArrayPath, "app/dir-link")); got != extBefore {
				t.Fatalf("dir symlink target = %q, want %q", got, extBefore)
			}
			if _, err := os.Lstat(filepath.Join(tree.externalDir, "outside.txt")); err != nil {
				t.Fatalf("a symlink was followed and its target removed: %v", err)
			}
			assertGoneOrLeft(t, s.CachePath, movedEntries, false)
			assertGoneOrLeft(t, s.CachePath, []string{"app/runtime.sock"}, true)

			if len(report.Moved()) != len(movedEntries) {
				t.Fatalf("Moved() = %d entries, want %d: %+v", len(report.Moved()), len(movedEntries), report.Entries)
			}
			sock, ok := resultFor(report, "app/runtime.sock")
			if !ok || sock.Result != ResultSkippedSocket {
				t.Fatalf("socket entry = %+v, want %s", sock, ResultSkippedSocket)
			}
			if _, err := os.Lstat(filepath.Join(s.ArrayPath, "app/runtime.sock")); err == nil {
				t.Fatal("a socket was recreated on the target")
			}
			if got, _ := os.ReadFile(filepath.Join(s.ArrayPath, "vm.img")); len(got) != int(tree.sparseSize) || !bytes.HasPrefix(got, []byte("head")) || string(got[tree.sparseSize/2:tree.sparseSize/2+6]) != "middle" {
				t.Fatalf("sparse file content differs from the source")
			}
		})
	}
}

func TestRun_ReportsEachEntryTypeAndTheSplitHardLink(t *testing.T) {
	s := newShare(t, "appdata")
	buildEntryTree(t, s.CachePath)

	var logged []string
	hooks := RunHooks{Log: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }}
	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), hooks, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	log := strings.Join(logged, "\n")
	for _, want := range []string{
		"moved appdata/app/rel-link (symlink)",
		"moved appdata/app/queue (fifo)",
		"skipped_socket appdata/app/runtime.sock (socket): runtime-only",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("job log lacks %q:\n%s", want, log)
		}
	}
	var splits int
	for _, e := range report.Entries {
		if strings.HasPrefix(e.Path, "data/") && strings.Contains(e.Reason, "hard links") {
			splits++
		}
	}
	if splits != 1 {
		t.Fatalf("expected the hard-linked pair to be reported as split once, got %d: %+v", splits, report.Entries)
	}
	var a, b syscall.Stat_t
	if err := syscall.Stat(filepath.Join(s.ArrayPath, "data/original"), &a); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Stat(filepath.Join(s.ArrayPath, "data/alias"), &b); err != nil {
		t.Fatal(err)
	}
	if a.Ino == b.Ino {
		t.Fatal("expected the hard-linked pair to be two independent files on the target")
	}
}

func TestRun_SymlinkIsNotHeldOpenByItsTarget(t *testing.T) {
	s := newShare(t, "appdata")
	mustWrite(t, filepath.Join(s.CachePath, "db.sqlite"), "db")
	link := filepath.Join(s.CachePath, "current")
	if err := os.Symlink("db.sqlite", link); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, link, []unix.Timespec{unix.NsecToTimespec(old.UnixNano()), unix.NsecToTimespec(old.UnixNano())}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	open := NewFakeOpenChecker()
	open.SetOpen(filepath.Join(s.CachePath, "db.sqlite"), true)
	open.SetOpen(link, true)

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(open), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if e, _ := resultFor(report, "current"); e.Result != ResultMoved {
		t.Fatalf("symlink entry = %+v, want moved: a symlink cannot be held open", e)
	}
	if e, _ := resultFor(report, "db.sqlite"); e.Result != ResultSkippedOpen {
		t.Fatalf("open file entry = %+v, want skipped_open", e)
	}
}

func TestRun_MknodFailureIsAFailedEntryAndLeavesTheSource(t *testing.T) {
	s := newShare(t, "appdata")
	fifo := filepath.Join(s.CachePath, "queue")
	if err := os.MkdirAll(s.CachePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(fifo, 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(fifo, old, old); err != nil {
		t.Fatal(err)
	}
	deps := testDeps(NewFakeOpenChecker())
	deps.Mknod = func(int, string, uint32, int) error { return unix.EPERM }

	report, err := Run(context.Background(), []Share{s}, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	e, _ := resultFor(report, "queue")
	if e.Result != ResultFailed || !strings.Contains(e.Err, "operation not permitted") {
		t.Fatalf("entry = %+v, want failed with the mknod error", e)
	}
	if _, err := os.Lstat(fifo); err != nil {
		t.Fatalf("source FIFO must stay: %v", err)
	}
	left, _ := os.ReadDir(s.ArrayPath)
	if len(left) != 0 {
		t.Fatalf("target holds leftovers after a failed create: %v", left)
	}
}

func TestRun_PendingSymlinkCopyIsCompletedNotRecopied(t *testing.T) {
	s := newShare(t, "appdata")
	tree := buildEntryTree(t, s.CachePath)
	// An earlier run published the symlink's copy and stopped before unlink.
	if err := os.MkdirAll(filepath.Join(s.ArrayPath, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(tree.path("app/rel-link"))
	dst := filepath.Join(s.ArrayPath, "app/rel-link")
	if err := os.Symlink(readLinkOrFail(t, tree.path("app/rel-link")), dst); err != nil {
		t.Fatal(err)
	}
	ts := unix.NsecToTimespec(info.ModTime().UnixNano())
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, dst, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	e, _ := resultFor(report, "app/rel-link")
	if e.Result != ResultMoved || !strings.Contains(e.Reason, "pending relocation") {
		t.Fatalf("entry = %+v, want the pending relocation completed", e)
	}
}

func TestRun_SymlinkWithADifferentTargetOnTheArrayIsAConflict(t *testing.T) {
	s := newShare(t, "appdata")
	tree := buildEntryTree(t, s.CachePath)
	if err := os.MkdirAll(filepath.Join(s.ArrayPath, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(tree.path("app/rel-link"))
	dst := filepath.Join(s.ArrayPath, "app/rel-link")
	if err := os.Symlink("../other.txt", dst); err != nil {
		t.Fatal(err)
	}
	ts := unix.NsecToTimespec(info.ModTime().UnixNano())
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, dst, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if e, _ := resultFor(report, "app/rel-link"); e.Result != ResultConflict {
		t.Fatalf("entry = %+v, want a conflict", e)
	}
	assertGoneOrLeft(t, s.CachePath, []string{"app/rel-link"}, true)
	if got := readLinkOrFail(t, dst); got != "../other.txt" {
		t.Fatalf("the array's own symlink was replaced: %q", got)
	}
}

func TestRelocateToArray_ReportsOnlyRuntimeSocketsAsComplete(t *testing.T) {
	s := newShare(t, "appdata")
	buildEntryTree(t, s.CachePath)

	report, err := RelocateToArray(context.Background(), s, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToArray: %v", err)
	}
	if report.Incomplete() {
		t.Fatalf("only a socket is left behind, the relocation is complete: %+v", report.LeftBehind())
	}
	if !strings.Contains(report.Summary(), "1 runtime-only") {
		t.Fatalf("Summary() = %q, want the left-behind socket counted", report.Summary())
	}
}

func TestRelocateToArray_AnOpenFileMakesTheRelocationIncomplete(t *testing.T) {
	s := newShare(t, "appdata")
	tree := buildEntryTree(t, s.CachePath)
	open := NewFakeOpenChecker()
	open.SetOpen(tree.path("plain.txt"), true)

	report, err := RelocateToArray(context.Background(), s, Config{}, testDeps(open), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToArray: %v", err)
	}
	if !report.Incomplete() {
		t.Fatal("a file left behind must make the relocation incomplete")
	}
	var left []Entry
	for _, e := range report.LeftBehind() {
		if e.Result != ResultSkippedSocket {
			left = append(left, e)
		}
	}
	if len(left) != 1 || left[0].Path != "plain.txt" || left[0].Result != ResultSkippedOpen {
		t.Fatalf("LeftBehind() = %+v, want plain.txt (skipped_open) besides the socket", left)
	}
	if !strings.Contains(report.Summary(), "incomplete: 1 left behind") {
		t.Fatalf("Summary() = %q, want it to say the relocation is incomplete", report.Summary())
	}
}

func TestMover_ReportIsNotARelocation(t *testing.T) {
	s := newShare(t, "appdata")
	buildEntryTree(t, s.CachePath)
	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete() || strings.Contains(report.Summary(), "incomplete") {
		t.Fatalf("a scheduled mover pass is never reported as an incomplete relocation: %s", report.Summary())
	}
}

func TestRelocateToCache_CarriesEveryEntryTypeAndDeletesOnlyAfterTheSync(t *testing.T) {
	s := relocateShare(t, "appdata", 2)
	tree := buildEntryTree(t, s.Branches[0])
	snap := snapshotEntries(t, tree, movedEntries)

	deps := testDeps(NewFakeOpenChecker())
	var syncs int
	deps.Sync = func(_ context.Context, manifest []parity.ManifestEntry) error {
		syncs++
		if syncs == 1 {
			// Phase one: the copies exist and verify, nothing is deleted yet.
			assertGoneOrLeft(t, s.Branches[0], movedEntries, true)
			snap.assertOn(t, s.CachePath)
		}
		return nil
	}

	report, err := RelocateToCache(context.Background(), s, Config{VerifyChecksum: true}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	if syncs != 2 {
		t.Fatalf("syncs = %d, want a sync before the deletes and one after", syncs)
	}
	assertGoneOrLeft(t, s.Branches[0], movedEntries, false)
	assertGoneOrLeft(t, s.Branches[0], []string{"app/runtime.sock"}, true)
	if len(report.Moved()) != len(movedEntries) {
		t.Fatalf("Moved() = %d, want %d: %+v", len(report.Moved()), len(movedEntries), report.Entries)
	}
	if report.Incomplete() {
		t.Fatalf("only a socket is left behind: %+v", report.LeftBehind())
	}
	if e, _ := resultFor(report, "app/runtime.sock"); e.Result != ResultSkippedSocket {
		t.Fatalf("socket entry = %+v", e)
	}
	if _, err := os.Lstat(filepath.Join(tree.externalDir, "outside.txt")); err != nil {
		t.Fatalf("a symlink was followed: %v", err)
	}
}

func TestRelocateToCache_BlockedSyncKeepsEverySource(t *testing.T) {
	s := relocateShare(t, "appdata", 1)
	buildEntryTree(t, s.Branches[0])

	deps := testDeps(NewFakeOpenChecker())
	deps.Sync = func(context.Context, []parity.ManifestEntry) error { return parity.ErrGuardBlocked }

	if _, err := RelocateToCache(context.Background(), s, Config{}, deps, RunHooks{}, nil); !errors.Is(err, parity.ErrGuardBlocked) {
		t.Fatalf("RelocateToCache: %v, want the guard block", err)
	}
	assertGoneOrLeft(t, s.Branches[0], append([]string{"app/runtime.sock"}, movedEntries...), true)
}

func TestPlanEvacuation_MovesSymlinksAndFifosAndEmptiesTheDisk(t *testing.T) {
	base := t.TempDir()
	disk1, disk2 := filepath.Join(base, "disk1"), filepath.Join(base, "disk2")
	s := evacuateShare(t, "appdata", []string{disk1, disk2})
	tree := buildEntryTree(t, s.Branches[0])
	if err := os.Remove(tree.path("app/runtime.sock")); err != nil {
		t.Fatal(err)
	}
	snap := snapshotEntries(t, tree, movedEntries)

	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.Usage = fakeUsage(map[string]DiskUsage{s.Branches[1]: {TotalBytes: 1 << 40, FreeBytes: 1 << 39}})
	plan, err := PlanEvacuation(context.Background(), disk1, []Share{s}, deps)
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	if len(plan.Moves) != len(movedEntries) {
		t.Fatalf("plan holds %d moves, want one per entry (%d): %+v", len(plan.Moves), len(movedEntries), plan.Moves)
	}
	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	deps.Sync = syncFuncFromEngine(engine)

	report, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	if len(report.Moved()) != len(movedEntries) {
		t.Fatalf("Moved() = %d: %+v", len(report.Moved()), report.Entries)
	}
	snap.assertOn(t, s.Branches[1])
	if err := EvacuationPostCheck(disk1, []Share{s}); err != nil {
		t.Fatalf("EvacuationPostCheck: %v", err)
	}
}

func TestPlanEvacuation_RefusesASocketBeforeAnyCopy(t *testing.T) {
	base := t.TempDir()
	disk1, disk2 := filepath.Join(base, "disk1"), filepath.Join(base, "disk2")
	s := evacuateShare(t, "appdata", []string{disk1, disk2})
	rebalanceWriteSize(t, filepath.Join(s.Branches[0], "a.bin"), 100)
	buildEntryTree(t, s.Branches[0])

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Usage = fakeUsage(map[string]DiskUsage{s.Branches[1]: {TotalBytes: 1 << 40, FreeBytes: 1 << 39}})
	_, err := PlanEvacuation(context.Background(), disk1, []Share{s}, deps)
	if !errors.Is(err, ErrEvacuationUnsupportedEntry) || !strings.Contains(err.Error(), "runtime.sock") {
		t.Fatalf("PlanEvacuation: %v, want ErrEvacuationUnsupportedEntry naming the socket", err)
	}
}

func TestCopyRegular_KeepsHolesAndVerifiesTheFullContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	const size = 16 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	// Data at the start, in the middle, and a hole that runs to the end.
	for _, off := range []int64{0, 4 << 20, 9<<20 + 123} {
		if _, err := f.WriteAt(bytes.Repeat([]byte{0xAB}, 5000), off); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if blocks(t, src) > size/2 {
		t.Skip("the temp filesystem does not keep holes")
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(src)

	for _, verify := range []bool{false, true} {
		dst := filepath.Join(dir, fmt.Sprintf("dst-%v.img", verify))
		if err := copyMoveFile(src, dst, dir, info, Config{VerifyChecksum: verify}, testDeps(NewFakeOpenChecker()).withDefaults()); err != nil {
			t.Fatalf("copyMoveFile(verify=%v): %v", verify, err)
		}
		want, _ := os.ReadFile(src)
		got, _ := os.ReadFile(dst)
		if !bytes.Equal(want, got) {
			t.Fatalf("verify=%v: content differs", verify)
		}
		if b, sb := blocks(t, dst), blocks(t, src); b > sb+64<<10 {
			t.Fatalf("verify=%v: %d bytes allocated on the target, %d on the source", verify, b, sb)
		}
	}
}

func TestCopyRegular_FullyDenseFileStaysDense(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "dense")
	content := bytes.Repeat([]byte("0123456789abcdef"), 1<<16)
	if err := os.WriteFile(src, content, 0o640); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(src)
	dst := filepath.Join(dir, "out")
	if err := copyMoveFile(src, dst, dir, info, Config{VerifyChecksum: true}, testDeps(NewFakeOpenChecker()).withDefaults()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, content) {
		t.Fatal("content differs")
	}
}

func TestCopyRegular_NeverFollowsASymlinkSwappedInAfterTheStat(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("not for the array"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	mustWrite(t, src, "was a regular file")
	info, _ := os.Lstat(src)
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, src); err != nil {
		t.Fatal(err)
	}

	err := copyMoveFile(src, filepath.Join(dir, "dst"), dir, info, Config{}, testDeps(NewFakeOpenChecker()).withDefaults())
	if err == nil {
		t.Fatal("copyMoveFile followed a symlink that replaced the regular file")
	}
	if _, serr := os.Lstat(filepath.Join(dir, "dst")); serr == nil {
		t.Fatal("a copy of the symlink's target was published")
	}
}

func TestCopyRegular_PreallocatedFileStaysAllocated(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "prealloc.bin")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	const size = 4 << 20
	if err := unix.Fallocate(int(f.Fd()), 0, 0, size); err != nil {
		t.Skipf("fallocate unsupported here: %v", err)
	}
	_ = f.Close()
	if blocks(t, src) < size {
		t.Skip("the temp filesystem does not allocate on fallocate")
	}
	info, _ := os.Lstat(src)
	dst := filepath.Join(dir, "out.bin")
	if err := copyMoveFile(src, dst, dir, info, Config{}, testDeps(NewFakeOpenChecker()).withDefaults()); err != nil {
		t.Fatal(err)
	}
	if got := blocks(t, dst); got < size {
		t.Fatalf("a fully allocated source became %d allocated bytes on the target, want at least %d", got, size)
	}
}
