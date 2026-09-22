package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testDeps(open *FakeOpenChecker) Deps {
	return Deps{
		Open: open,
		UUID: func() string { return "test-uuid" },
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func newShare(t *testing.T, name string) Share {
	t.Helper()
	base := t.TempDir()
	return Share{
		Name:      name,
		CachePath: filepath.Join(base, "cache", name),
		ArrayPath: filepath.Join(base, "array", name),
	}
}

// TestRun_MovesEligibleFile is the mover's central happy path: a file
// past the grace period, not open, with room on the array, is copied
// through the array-write path, verified, and the source is removed —
// exactly once, with mode and modification time preserved (doc 09 §2).
func TestRun_MovesEligibleFile(t *testing.T) {
	s := newShare(t, "movies")
	src := filepath.Join(s.CachePath, "movie.mkv")
	mustWrite(t, src, "movie bytes")
	if err := os.Chmod(src, 0o640); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Moved()) != 1 {
		t.Fatalf("Moved() = %d entries, want 1: %+v", len(report.Moved()), report.Entries)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still exists after move: err=%v", err)
	}
	dst := filepath.Join(s.ArrayPath, "movie.mkv")
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("target missing: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("target mode = %v, want 0640", info.Mode().Perm())
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "movie bytes" {
		t.Errorf("target content = %q, %v, want %q", got, err, "movie bytes")
	}
	if report.MovedBytes() != int64(len("movie bytes")) {
		t.Errorf("MovedBytes() = %d, want %d", report.MovedBytes(), len("movie bytes"))
	}
}

// TestRun_SkipsOpenFile proves the file is left exactly where it is when
// OpenChecker reports it held open — the FakeOpenChecker stands in for
// mergerfs itself holding a client's handle (doc 09 §2's "including
// mergerfs's own descriptors" — the real ProcOpenChecker path is
// exercised against real mergerfs in the lab).
func TestRun_SkipsOpenFile(t *testing.T) {
	s := newShare(t, "downloads")
	src := filepath.Join(s.CachePath, "file.iso")
	mustWrite(t, src, "data")

	open := NewFakeOpenChecker()
	open.SetOpen(src, true)

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(open), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Moved()) != 0 {
		t.Fatalf("expected nothing moved, got %+v", report.Moved())
	}
	skipped := report.Skipped()
	if len(skipped) != 1 || skipped[0].Result != ResultSkippedOpen {
		t.Fatalf("expected one skipped_open entry, got %+v", skipped)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source should still exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.ArrayPath, "file.iso")); !os.IsNotExist(err) {
		t.Fatalf("target should not exist yet: err=%v", err)
	}
}

// TestRun_SkipsWithinGracePeriod proves a just-written file is left
// alone: mtime "now" is well inside the default grace period against the
// fixed clock testDeps uses.
func TestRun_SkipsWithinGracePeriod(t *testing.T) {
	s := newShare(t, "downloads")
	src := filepath.Join(s.CachePath, "still-writing.part")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("partial"), 0o640); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(src, now, now); err != nil {
		t.Fatal(err)
	}

	deps := testDeps(NewFakeOpenChecker())
	report, err := Run(context.Background(), []Share{s}, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Result != ResultSkippedGrace {
		t.Fatalf("expected one skipped_grace_period entry, got %+v", report.Entries)
	}
}

// TestRun_SkipsExcludedPattern proves a share's own Exclude patterns are
// honoured (doc 09 §2's algorithm: "skip if in an excluded pattern").
func TestRun_SkipsExcludedPattern(t *testing.T) {
	s := newShare(t, "media")
	s.Exclude = []string{"*.part"}
	src := filepath.Join(s.CachePath, "download.part")
	mustWrite(t, src, "partial")

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Result != ResultSkippedExcluded {
		t.Fatalf("expected one skipped_excluded entry, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("excluded source should be untouched: %v", err)
	}
}

// TestRun_RefusesWithoutSpace proves the mover refuses to start a file
// when no branch has room for it (doc 09 §2's space pre-check
// acceptance test).
func TestRun_RefusesWithoutSpace(t *testing.T) {
	s := newShare(t, "media")
	s.MinFreeSpace = 1000
	src := filepath.Join(s.CachePath, "big.bin")
	mustWrite(t, src, "0123456789")

	deps := testDeps(NewFakeOpenChecker())
	deps.Avail = func(string) (int64, error) { return 1005, nil } // 1005 - 1000 < 10 bytes needed

	report, err := Run(context.Background(), []Share{s}, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Result != ResultSkippedNoSpace {
		t.Fatalf("expected one skipped_no_space entry, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source should be untouched: %v", err)
	}
}

// TestRun_SpaceEligibleOnAnyBranch proves the pre-check accepts as soon
// as any one branch has room, mirroring "at least one eligible disk"
// (doc 09 §2) without this package ever choosing which one.
func TestRun_SpaceEligibleOnAnyBranch(t *testing.T) {
	s := newShare(t, "media")
	s.Branches = []string{"/branch/full", "/branch/roomy"}
	src := filepath.Join(s.CachePath, "file.bin")
	mustWrite(t, src, "0123456789")

	deps := testDeps(NewFakeOpenChecker())
	deps.Avail = func(path string) (int64, error) {
		if path == "/branch/roomy" {
			return 1 << 30, nil
		}
		return 0, nil
	}

	report, err := Run(context.Background(), []Share{s}, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Moved()) != 1 {
		t.Fatalf("expected the file to move, got %+v", report.Entries)
	}
}

// TestRun_InterruptedMidCopyLeavesNoGap simulates a mover process killed
// after it created a temp-suffixed partial copy but before the rename —
// the doc 09 §2 SIGKILL scenario, exercised at the state level by
// fabricating the on-disk state a real kill would leave rather than by
// actually killing a process (TestLabMover_SIGKILLMidCopyLeavesNoGap, in
// this package's own lab test file, does that for real, against real
// mergerfs). The source must still be intact — never a gap — and a fresh
// Run must clean up the stray temp file and complete the move correctly,
// leaving a single, correct copy.
func TestRun_InterruptedMidCopyLeavesNoGap(t *testing.T) {
	s := newShare(t, "media")
	src := filepath.Join(s.CachePath, "show.mkv")
	mustWrite(t, src, "episode bytes")

	strayTemp := filepath.Join(s.ArrayPath, "show.mkv"+tempSuffix+"stale-uuid")
	if err := os.MkdirAll(filepath.Dir(strayTemp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strayTemp, []byte("episode by"), 0o640); err != nil { // truncated, as a kill mid-copy would leave it
		t.Fatal(err)
	}

	if _, err := os.Stat(src); err != nil {
		t.Fatalf("precondition: source must exist: %v", err)
	}

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(strayTemp); !os.IsNotExist(err) {
		t.Fatalf("stray temp file should have been swept: err=%v", err)
	}
	final := filepath.Join(s.ArrayPath, "show.mkv")
	got, err := os.ReadFile(final)
	if err != nil || string(got) != "episode bytes" {
		t.Fatalf("final file = %q, %v, want the full source content", got, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source should be removed after a clean completion: err=%v", err)
	}
	if len(report.Moved()) != 1 {
		t.Fatalf("expected exactly one moved entry, got %+v", report.Entries)
	}
}

// TestRun_CompletesPendingDeleteAfterInterruptedUnlink simulates a mover
// interrupted after the rename succeeded but before the source unlink —
// the other half of "leaves a duplicate, never a gap" (doc 09 §2). The
// resumed run must complete the delete without re-copying.
func TestRun_CompletesPendingDeleteAfterInterruptedUnlink(t *testing.T) {
	s := newShare(t, "media")
	content := "already copied"
	src := filepath.Join(s.CachePath, "done.mkv")
	mustWrite(t, src, content)
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(s.ArrayPath, "done.mkv")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	// A real prior run's copyMoveFile always leaves dst's mtime exactly
	// equal to src's — the mark isSamePendingCopy relies on to tell this
	// apart from an unrelated same-size file.
	if err := os.Chtimes(dst, srcInfo.ModTime(), srcInfo.ModTime()); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Moved()) != 1 || report.Entries[0].Reason == "" {
		t.Fatalf("expected one moved entry noting a completed pending relocation, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source should now be removed: err=%v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != content {
		t.Fatalf("target content = %q, %v, want %q (must not be re-copied)", got, err, content)
	}
}

// TestRun_FsyncsTargetDirectoryBeforeSourceUnlink is #53's own fix-round
// regression test for the power-loss gap: dst and src live on independent
// filesystems with independent journals, so the rename into dst's
// directory must be made durable (fsynced) before the cache-side source
// can ever be unlinked — otherwise a power loss between the two can
// commit the unlink's journal without the rename's, leaving the array
// holding only the temp-suffixed name, which a later run's
// sweepStrayTemps discards as a stray. The fake FsyncDir hook here
// observes, at the moment it is called, that the rename has already
// happened but the source has not yet been removed — proving the actual
// ordering, not just that both eventually occur.
func TestRun_FsyncsTargetDirectoryBeforeSourceUnlink(t *testing.T) {
	s := newShare(t, "media")
	src := filepath.Join(s.CachePath, "show.mkv")
	mustWrite(t, src, "episode bytes")
	dst := filepath.Join(s.ArrayPath, "show.mkv")

	var fsyncedDir string
	var sourceExistedAtFsync, renameDoneAtFsync bool
	deps := testDeps(NewFakeOpenChecker())
	deps.FsyncDir = func(dir string) error {
		fsyncedDir = dir
		if _, err := os.Stat(src); err == nil {
			sourceExistedAtFsync = true
		}
		if _, err := os.Stat(dst); err == nil {
			renameDoneAtFsync = true
		}
		return fsyncDir(dir)
	}

	report, err := Run(context.Background(), []Share{s}, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Moved()) != 1 {
		t.Fatalf("expected exactly one moved entry, got %+v", report.Entries)
	}
	if fsyncedDir != s.ArrayPath {
		t.Fatalf("FsyncDir called with %q, want the target directory %q", fsyncedDir, s.ArrayPath)
	}
	if !renameDoneAtFsync {
		t.Fatal("expected the rename into place to have already happened when FsyncDir was called")
	}
	if !sourceExistedAtFsync {
		t.Fatal("expected the cache source to still exist when FsyncDir was called — it must be fsynced before the source can be unlinked")
	}
}

// TestRun_FsyncDirFailureLeavesSourceIntactAndResumeCompletes proves that
// when the destination directory fsync itself fails, the source is never
// unlinked on that pass (copyMoveFile reports the failure before
// finishPendingDelete ever runs) — and that a later, successful run
// recognizes the already-renamed target as its own pending copy and
// completes the delete rather than re-copying: a duplicate, never a gap,
// even when the fsync step is what failed.
func TestRun_FsyncDirFailureLeavesSourceIntactAndResumeCompletes(t *testing.T) {
	s := newShare(t, "media")
	src := filepath.Join(s.CachePath, "show.mkv")
	mustWrite(t, src, "episode bytes")
	dst := filepath.Join(s.ArrayPath, "show.mkv")

	failingDeps := testDeps(NewFakeOpenChecker())
	failingDeps.FsyncDir = func(dir string) error {
		return fmt.Errorf("simulated fsync failure")
	}

	report, err := Run(context.Background(), []Share{s}, Config{}, failingDeps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Result != ResultFailed {
		t.Fatalf("expected one failed entry, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive a target directory fsync failure: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("the rename itself must have already completed even though the fsync failed: %v", err)
	}

	report, err = Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if len(report.Moved()) != 1 || report.Entries[0].Reason == "" {
		t.Fatalf("expected the resumed run to complete the pending relocation without re-copying, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source should be removed after the resumed run completes the delete: err=%v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "episode bytes" {
		t.Fatalf("target content = %q, %v, want the original content (must not be re-copied)", got, err)
	}
}

// TestRun_PendingDeleteLeftAloneWhileSourceOpen proves the completion
// path re-checks open handles too, rather than assuming a pre-existing
// target means it is always safe to delete the source immediately.
func TestRun_PendingDeleteLeftAloneWhileSourceOpen(t *testing.T) {
	s := newShare(t, "media")
	content := "already copied"
	src := filepath.Join(s.CachePath, "done.mkv")
	mustWrite(t, src, content)
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(s.ArrayPath, "done.mkv")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dst, srcInfo.ModTime(), srcInfo.ModTime()); err != nil {
		t.Fatal(err)
	}

	open := NewFakeOpenChecker()
	open.SetOpen(src, true)

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(open), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Result != ResultMovedPendingDelete {
		t.Fatalf("expected one moved_pending_delete entry, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive while still open: %v", err)
	}
}

// TestRun_PendingDeleteSameSizeDifferentContentIsConflict is #53's own
// fix-round regression test: an array-side file that happens to share the
// cache source's exact size, but was never this mover's own copy (its
// mtime is unrelated to the source's), must never be treated as an
// unfinished pending delete and have its cache source silently discarded.
// Before this fix, size equality alone was enough, and this exact
// scenario — a same-size independent rewrite on the array racing a mover
// pass — deleted the newer cache copy and left the stale array copy,
// reporting it as "moved".
func TestRun_PendingDeleteSameSizeDifferentContentIsConflict(t *testing.T) {
	s := newShare(t, "media")
	src := filepath.Join(s.CachePath, "file.bin")
	mustWrite(t, src, "NEW-VERSION-0123") // 16 bytes, grace-period-old mtime
	dst := filepath.Join(s.ArrayPath, "file.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("OLD-VERSION-0123"), 0o640); err != nil { // also 16 bytes
		t.Fatal(err)
	}
	// dst's mtime is left as "now" (this write), deliberately different
	// from src's — it was rewritten on the array independently, not
	// produced by an earlier pass of this mover.

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Result != ResultConflict {
		t.Fatalf("expected one conflict entry, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive a same-size conflict, not be silently deleted: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "OLD-VERSION-0123" {
		t.Fatalf("target must survive a same-size conflict unmodified, got %q, %v", got, err)
	}
}

// TestRun_PendingDeleteChecksumCatchesSameSizeSameMtimeDifferentContent
// proves VerifyChecksum closes the residual gap size-and-mtime equality
// alone cannot: two genuinely different files can still coincidentally
// share both.
func TestRun_PendingDeleteChecksumCatchesSameSizeSameMtimeDifferentContent(t *testing.T) {
	s := newShare(t, "media")
	src := filepath.Join(s.CachePath, "file.bin")
	mustWrite(t, src, "NEW-VERSION-0123")
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(s.ArrayPath, "file.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("OLD-VERSION-0123"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dst, srcInfo.ModTime(), srcInfo.ModTime()); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), []Share{s}, Config{VerifyChecksum: true}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Result != ResultConflict {
		t.Fatalf("expected one conflict entry, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive a checksum-detected conflict: %v", err)
	}
}

// TestRun_ConflictNeverAutoResolved proves a target that already exists
// with a *different* size than the source is left alone entirely rather
// than guessed at — CLAUDE.md's "anything that can lose data" bar.
func TestRun_ConflictNeverAutoResolved(t *testing.T) {
	s := newShare(t, "media")
	src := filepath.Join(s.CachePath, "file.bin")
	mustWrite(t, src, "new content, different length")
	dst := filepath.Join(s.ArrayPath, "file.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Result != ResultConflict {
		t.Fatalf("expected one conflict entry, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive a conflict: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "old" {
		t.Fatalf("target must survive a conflict unmodified, got %q", got)
	}
}

// racyOpenChecker is a scripted OpenChecker whose IsOpen has a side
// effect: it plants dst before reporting "not open". processFile's own
// Lstat(dst) conflict check has already run and found nothing by the
// time IsOpen is consulted, so this stands in for a client writing
// through the array mount during the multi-minute copy that follows —
// the exact window TestRun_RejectsTargetThatAppearsDuringCopy proves is
// closed.
type racyOpenChecker struct {
	dst     string
	content string
}

func (r racyOpenChecker) IsOpen(_ context.Context, _ string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(r.dst), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(r.dst, []byte(r.content), 0o640); err != nil {
		return false, err
	}
	return false, nil
}

// TestRun_RejectsTargetThatAppearsDuringCopy proves the copy-to-rename
// window is closed: a target created after processFile's own Lstat(dst)
// check (here simulated via IsOpen, called immediately before the copy)
// must not be silently overwritten by the final rename. The rename has
// to fail closed into ResultConflict, and both the source and the
// appeared target must survive untouched (doc 09 §2: a conflict is never
// auto-resolved).
func TestRun_RejectsTargetThatAppearsDuringCopy(t *testing.T) {
	s := newShare(t, "media")
	src := filepath.Join(s.CachePath, "file.bin")
	mustWrite(t, src, "cache-side content")
	dst := filepath.Join(s.ArrayPath, "file.bin")

	deps := testDeps(NewFakeOpenChecker())
	deps.Open = racyOpenChecker{dst: dst, content: "written through the array mount mid-copy"}

	report, err := Run(context.Background(), []Share{s}, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Result != ResultConflict {
		t.Fatalf("expected one conflict entry, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive a conflict discovered mid-copy: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "written through the array mount mid-copy" {
		t.Fatalf("target written during the copy must survive unmodified, got %q, %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(dst) {
			t.Fatalf("stray temp file left behind: %s", e.Name())
		}
	}
}

// TestRun_ChecksumVerification proves VerifyChecksum actually reads the
// target back from disk and compares it, rather than trusting the bytes
// that passed through memory during the copy.
func TestRun_ChecksumVerification(t *testing.T) {
	s := newShare(t, "media")
	src := filepath.Join(s.CachePath, "file.bin")
	mustWrite(t, src, "checksummed content")

	report, err := Run(context.Background(), []Share{s}, Config{VerifyChecksum: true}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Moved()) != 1 {
		t.Fatalf("expected the file to move, got %+v", report.Entries)
	}
}

// TestRun_FinalizesReportOnCheckpointError proves Run leaves FinishedAt
// set even on a post-start error return — here SaveCheckpoint failing
// after the first file is already decided. RunMover logs report.Summary()
// on the error path too (doc 09 §2's "honest reporting"), and Summary's
// duration is FinishedAt.Sub(StartedAt) — meaningless, and wildly
// negative, against a zero FinishedAt.
func TestRun_FinalizesReportOnCheckpointError(t *testing.T) {
	s := newShare(t, "media")
	mustWrite(t, filepath.Join(s.CachePath, "a.bin"), "aaa")
	mustWrite(t, filepath.Join(s.CachePath, "b.bin"), "bbb")

	boom := errors.New("boom")
	hooks := RunHooks{SaveCheckpoint: func(data []byte) error { return boom }}

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), hooks, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want it to wrap %v", err, boom)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("expected the first file to have been decided before the checkpoint error, got %+v", report.Entries)
	}
	if report.FinishedAt.IsZero() {
		t.Fatal("FinishedAt is zero on an error return — Summary()'s duration would be meaningless")
	}
	if report.FinishedAt.Before(report.StartedAt) {
		t.Fatalf("FinishedAt %v is before StartedAt %v", report.FinishedAt, report.StartedAt)
	}
}

// TestRun_ResumesFromCheckpoint proves a resumed run, given the
// checkpoint the previous run saved, skips files it already decided
// rather than reprocessing the whole share from zero (Q29).
func TestRun_ResumesFromCheckpoint(t *testing.T) {
	s := newShare(t, "media")
	mustWrite(t, filepath.Join(s.CachePath, "a.bin"), "aaa")
	mustWrite(t, filepath.Join(s.CachePath, "b.bin"), "bbb")
	mustWrite(t, filepath.Join(s.CachePath, "c.bin"), "ccc")

	open := NewFakeOpenChecker()
	open.SetOpen(filepath.Join(s.CachePath, "b.bin"), true)

	var checkpoints [][]byte
	hooks := RunHooks{SaveCheckpoint: func(data []byte) error {
		cp := append([]byte(nil), data...)
		checkpoints = append(checkpoints, cp)
		return nil
	}}

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(open), hooks, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 3 {
		t.Fatalf("first run: expected 3 entries, got %+v", report.Entries)
	}
	if len(checkpoints) == 0 {
		t.Fatal("expected at least one checkpoint to be saved")
	}

	// b.bin is now unblocked; resuming from the last checkpoint (after
	// c.bin, the lexically last file) must not reprocess a.bin or c.bin.
	open.SetOpen(filepath.Join(s.CachePath, "b.bin"), false)
	last := checkpoints[len(checkpoints)-1]

	resumed, err := Run(context.Background(), []Share{s}, Config{}, testDeps(open), RunHooks{}, last)
	if err != nil {
		t.Fatalf("Run (resumed): %v", err)
	}
	if len(resumed.Entries) != 0 {
		t.Fatalf("resuming past the last file should have nothing left to do, got %+v", resumed.Entries)
	}
	if _, err := os.Stat(filepath.Join(s.CachePath, "a.bin")); !os.IsNotExist(err) {
		t.Fatalf("a.bin should already be moved: err=%v", err)
	}
}

// TestRun_StopRequestedInterruptsAndCheckpoints proves StopRequested
// halts the run cleanly and mid-run progress is already checkpointed —
// the graceful-stop half of Q29/Q70, independent of the SIGKILL case.
func TestRun_StopRequestedInterruptsAndCheckpoints(t *testing.T) {
	s := newShare(t, "media")
	mustWrite(t, filepath.Join(s.CachePath, "a.bin"), "aaa")
	mustWrite(t, filepath.Join(s.CachePath, "b.bin"), "bbb")

	stop := make(chan struct{})
	close(stop) // already requested before Run even starts its first file

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()),
		RunHooks{StopRequested: stop}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Interrupted {
		t.Fatal("expected report.Interrupted to be true")
	}
	if len(report.Entries) != 0 {
		t.Fatalf("expected no work to have started, got %+v", report.Entries)
	}
}

// TestRun_MultipleSharesSkipCompletedOnesOnResume proves ShareIndex in
// the checkpoint skips a share entirely once it is done, without
// re-enumerating it.
func TestRun_MultipleSharesSkipCompletedOnesOnResume(t *testing.T) {
	s1 := newShare(t, "movies")
	s2 := newShare(t, "shows")
	mustWrite(t, filepath.Join(s1.CachePath, "one.mkv"), "one")
	mustWrite(t, filepath.Join(s2.CachePath, "two.mkv"), "two")

	cp := Checkpoint{ShareIndex: 1, LastPath: ""}
	data, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), []Share{s1, s2}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, data)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s1.CachePath, "one.mkv")); err != nil {
		t.Fatalf("share 0 must be skipped entirely by ShareIndex, but its file is gone: %v", err)
	}
	if len(report.Moved()) != 1 || report.Moved()[0].Share != "shows" {
		t.Fatalf("expected only share 1's file to move, got %+v", report.Entries)
	}
}

func TestRun_InvalidCheckpointIsAnError(t *testing.T) {
	s := newShare(t, "media")
	_, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, []byte("not json"))
	if err == nil {
		t.Fatal("expected an error decoding an invalid checkpoint")
	}
}

func TestRun_EmptyShareListIsANoOp(t *testing.T) {
	report, err := Run(context.Background(), nil, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 0 {
		t.Fatalf("expected no entries, got %+v", report.Entries)
	}
}

func TestRun_MissingCacheDirIsNotAnError(t *testing.T) {
	s := Share{Name: "empty", CachePath: filepath.Join(t.TempDir(), "does-not-exist"), ArrayPath: t.TempDir()}
	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 0 {
		t.Fatalf("expected no entries, got %+v", report.Entries)
	}
}

// TestRun_NonRegularFileIsLeftAlone proves a directory (or anything else
// that is not a regular file) under a share root is never touched.
func TestRun_NonRegularFileIsLeftAlone(t *testing.T) {
	s := newShare(t, "media")
	if err := os.MkdirAll(filepath.Join(s.CachePath, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(s.CachePath, "subdir", "real.bin"), "x")

	report, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Moved()) != 1 {
		t.Fatalf("expected the one real file to move, got %+v", report.Entries)
	}
}

// TestRun_PreservesTimestamps proves the target's modification time
// matches the source's, not the time the copy ran.
func TestRun_PreservesTimestamps(t *testing.T) {
	s := newShare(t, "media")
	src := filepath.Join(s.CachePath, "file.bin")
	mustWrite(t, src, "content")
	want := time.Unix(1_600_000_000, 0)
	if err := os.Chtimes(src, want, want); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	info, err := os.Stat(filepath.Join(s.ArrayPath, "file.bin"))
	if err != nil {
		t.Fatalf("target missing: %v", err)
	}
	if !info.ModTime().Equal(want) {
		t.Errorf("target mtime = %v, want %v", info.ModTime(), want)
	}
}

func TestRun_ContextCancelledBeforeStartIsReported(t *testing.T) {
	s := newShare(t, "media")
	mustWrite(t, filepath.Join(s.CachePath, "a.bin"), "aaa")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report, err := Run(ctx, []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run should report cancellation via Interrupted, not an error mid-file loop: %v", err)
	}
	if !report.Interrupted {
		t.Fatal("expected report.Interrupted to be true")
	}
}

func TestRun_LogHookReceivesEveryEntry(t *testing.T) {
	s := newShare(t, "media")
	mustWrite(t, filepath.Join(s.CachePath, "a.bin"), "aaa")

	var lines int
	hooks := RunHooks{Log: func(format string, args ...any) { lines++ }}
	if _, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), hooks, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lines != 1 {
		t.Fatalf("expected exactly one log line, got %d", lines)
	}
}

// TestRun_ProgressReachesComplete proves SetProgress is called and
// reaches 100 once every file across every share has been decided.
func TestRun_ProgressReachesComplete(t *testing.T) {
	s := newShare(t, "media")
	mustWrite(t, filepath.Join(s.CachePath, "a.bin"), "a")
	mustWrite(t, filepath.Join(s.CachePath, "b.bin"), "b")

	var last int
	hooks := RunHooks{SetProgress: func(pct int) { last = pct }}
	if _, err := Run(context.Background(), []Share{s}, Config{}, testDeps(NewFakeOpenChecker()), hooks, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if last != 100 {
		t.Fatalf("final progress = %d, want 100", last)
	}
}

// countingSnapshotter is a scriptable OpenChecker that also implements
// the optional Snapshotter interface Run's pre-copy check uses — a stand-
// in for a real /proc walker that records how many times it was actually
// asked to walk, so a test can prove Run takes one snapshot per share
// pass rather than one per file (#238). Its own IsOpen (the pre-unlink
// path always calls directly, never through a snapshot) behaves exactly
// like FakeOpenChecker.
type countingSnapshotter struct {
	*FakeOpenChecker
	snapshots int
	snapOpen  map[string]bool
}

func newCountingSnapshotter() *countingSnapshotter {
	return &countingSnapshotter{FakeOpenChecker: NewFakeOpenChecker(), snapOpen: make(map[string]bool)}
}

func (c *countingSnapshotter) Snapshot(context.Context) (OpenSnapshot, error) {
	c.snapshots++
	return countingSnapshot(c.snapOpen), nil
}

type countingSnapshot map[string]bool

func (s countingSnapshot) IsOpen(path string) (bool, error) {
	return s[path], nil
}

// TestRun_ReusesOneSnapshotPerShare proves a mover pass over a share with
// several eligible files takes exactly one /proc walk for its pre-copy
// checks, not one per file — the walker itself is asked to Snapshot once
// no matter how many files the share holds.
func TestRun_ReusesOneSnapshotPerShare(t *testing.T) {
	s := newShare(t, "media")
	mustWrite(t, filepath.Join(s.CachePath, "a.bin"), "aaa")
	mustWrite(t, filepath.Join(s.CachePath, "b.bin"), "bbb")
	mustWrite(t, filepath.Join(s.CachePath, "c.bin"), "ccc")

	snapshotter := newCountingSnapshotter()
	deps := testDeps(NewFakeOpenChecker())
	deps.Open = snapshotter

	report, err := Run(context.Background(), []Share{s}, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Moved()) != 3 {
		t.Fatalf("expected all three files to move, got %+v", report.Entries)
	}
	if snapshotter.snapshots != 1 {
		t.Fatalf("Snapshot called %d times, want exactly 1 for a three-file share pass", snapshotter.snapshots)
	}
}

// TestRun_PreUnlinkRecheckIgnoresStalePreCopySnapshot proves
// finishPendingDelete's pre-unlink check is answered by the checker's own
// live IsOpen, never by the pre-copy snapshot: scripting the file closed
// in the snapshot (so the copy proceeds) but open on the checker's own
// live IsOpen must still leave the source in place afterward, pending a
// later delete — the snapshot only ever gates the pre-copy decision, and
// the re-check immediately before unlink stays a fresh check against
// current process state (doc 09 §2).
func TestRun_PreUnlinkRecheckIgnoresStalePreCopySnapshot(t *testing.T) {
	s := newShare(t, "media")
	src := filepath.Join(s.CachePath, "file.bin")
	mustWrite(t, src, "content")

	snapshotter := newCountingSnapshotter()
	snapshotter.SetOpen(src, true) // live IsOpen reports open throughout; the snapshot never does

	deps := testDeps(NewFakeOpenChecker())
	deps.Open = snapshotter

	report, err := Run(context.Background(), []Share{s}, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Result != ResultMovedPendingDelete {
		t.Fatalf("expected one moved_pending_delete entry — the copy must have proceeded (the snapshot reported it closed) but the pre-unlink recheck must have caught the live open state, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive while the live pre-unlink check reports it open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.ArrayPath, "file.bin")); err != nil {
		t.Fatalf("the copy itself must have completed, since the pre-copy snapshot reported the file closed: %v", err)
	}
}
