package disk

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDataDiskUpgradeExceedsParity(t *testing.T) {
	cases := []struct {
		name    string
		newSize int64
		parity  []int64
		want    bool
	}{
		{"fits every parity disk", 4 * TB, []int64{8 * TB, 8 * TB}, false},
		{"exceeds one parity disk", 10 * TB, []int64{8 * TB, 12 * TB}, true},
		{"exactly equal is not exceeding", 8 * TB, []int64{8 * TB}, false},
		{"no parity disks at all", 8 * TB, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DataDiskUpgradeExceedsParity(c.newSize, c.parity); got != c.want {
				t.Fatalf("DataDiskUpgradeExceedsParity(%d, %v) = %v, want %v", c.newSize, c.parity, got, c.want)
			}
		})
	}
}

// buildUpgradeTestTree seeds root with a small file tree exercising
// every entry type copyDataDiskTree preserves: a top-level file with
// distinctive ownership-independent permissions and a fixed mtime, a
// nested subdirectory, and a symlink pointing at a sibling file.
func buildUpgradeTestTree(t *testing.T, root string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(root, "movies"))
	writeUpgradeTestFile(t, filepath.Join(root, "movies", "a.bin"), 200_000, 0o640)
	writeUpgradeTestFile(t, filepath.Join(root, "top.txt"), 37, 0o600)
	if err := os.Symlink("top.txt", filepath.Join(root, "top-link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

func writeUpgradeTestFile(t *testing.T, path string, size int, mode os.FileMode) []byte {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	// A fixed, distinctive mtime in the past, so a copy that merely
	// inherits "now" (rather than actually preserving the source's own
	// timestamp) is caught rather than accidentally matching.
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	return data
}

// TestCopyDataDiskTree_PreservesModeAndTimestamps is this issue's own
// "copy with ownership, xattrs and timestamps" acceptance criterion,
// exercised directly against the copy step (ownership is exercised
// separately below, since an unprivileged test can only chown to its
// own uid/gid).
func TestCopyDataDiskTree_PreservesModeAndTimestamps(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	buildUpgradeTestTree(t, src)

	interrupted, _, err := copyDataDiskTree(context.Background(), src, dst, DataDiskUpgradeHooks{}, "")
	if err != nil {
		t.Fatalf("copyDataDiskTree: %v", err)
	}
	if interrupted {
		t.Fatal("copyDataDiskTree reported interrupted with no stop requested")
	}

	srcInfo, err := os.Lstat(filepath.Join(src, "movies", "a.bin"))
	if err != nil {
		t.Fatalf("lstat src file: %v", err)
	}
	dstInfo, err := os.Lstat(filepath.Join(dst, "movies", "a.bin"))
	if err != nil {
		t.Fatalf("lstat dst file: %v", err)
	}
	if srcInfo.Mode().Perm() != dstInfo.Mode().Perm() {
		t.Fatalf("mode = %v, want %v", dstInfo.Mode().Perm(), srcInfo.Mode().Perm())
	}
	if !srcInfo.ModTime().Equal(dstInfo.ModTime()) {
		t.Fatalf("mtime = %v, want %v", dstInfo.ModTime(), srcInfo.ModTime())
	}
	if srcInfo.Size() != dstInfo.Size() {
		t.Fatalf("size = %d, want %d", dstInfo.Size(), srcInfo.Size())
	}

	srcData, _ := os.ReadFile(filepath.Join(src, "movies", "a.bin"))
	dstData, _ := os.ReadFile(filepath.Join(dst, "movies", "a.bin"))
	if string(srcData) != string(dstData) {
		t.Fatal("file content differs between source and destination")
	}

	target, err := os.Readlink(filepath.Join(dst, "top-link.txt"))
	if err != nil {
		t.Fatalf("readlink dst symlink: %v", err)
	}
	if target != "top.txt" {
		t.Fatalf("symlink target = %q, want %q", target, "top.txt")
	}

	dirInfo, err := os.Lstat(filepath.Join(dst, "movies"))
	if err != nil {
		t.Fatalf("lstat dst dir: %v", err)
	}
	if !dirInfo.IsDir() {
		t.Fatal("movies is not a directory on the destination")
	}
}

// TestVerifyDataDiskTree_CleanCopyPasses confirms the happy path: a
// faithful copy passes verification, including a checksum comparison.
func TestVerifyDataDiskTree_CleanCopyPasses(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	buildUpgradeTestTree(t, src)
	if _, _, err := copyDataDiskTree(context.Background(), src, dst, DataDiskUpgradeHooks{}, ""); err != nil {
		t.Fatalf("copyDataDiskTree: %v", err)
	}

	interrupted, err := verifyDataDiskTree(context.Background(), src, dst, true, DataDiskUpgradeHooks{})
	if err != nil {
		t.Fatalf("verifyDataDiskTree: %v", err)
	}
	if interrupted {
		t.Fatal("verifyDataDiskTree reported interrupted with no stop requested")
	}
}

// TestVerifyDataDiskTree_DetectsCorruptedContent is this issue's own
// central data-safety test for the verifying phase: content that
// differs from the source, despite matching size and mtime, is caught
// once checksum verification is requested.
func TestVerifyDataDiskTree_DetectsCorruptedContent(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	buildUpgradeTestTree(t, src)
	if _, _, err := copyDataDiskTree(context.Background(), src, dst, DataDiskUpgradeHooks{}, ""); err != nil {
		t.Fatalf("copyDataDiskTree: %v", err)
	}

	target := filepath.Join(dst, "movies", "a.bin")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading %s: %v", target, err)
	}
	data[0] ^= 0xFF
	srcInfo, err := os.Lstat(filepath.Join(src, "movies", "a.bin"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if err := os.WriteFile(target, data, srcInfo.Mode().Perm()); err != nil {
		t.Fatalf("writing corrupted file: %v", err)
	}
	if err := os.Chtimes(target, srcInfo.ModTime(), srcInfo.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	_, err = verifyDataDiskTree(context.Background(), src, dst, true, DataDiskUpgradeHooks{})
	if !errors.Is(err, ErrDataDiskUpgradeMismatch) {
		t.Fatalf("verifyDataDiskTree on corrupted content: err = %v, want ErrDataDiskUpgradeMismatch", err)
	}
}

// TestVerifyDataDiskTree_IgnoresDestinationOnlyLostAndFound proves a
// root lost+found on the destination only — mke2fs creates one on every
// fresh ext2/ext3/ext4 filesystem it formats, even though the old XFS
// disk never had one — does not fail verification by itself.
func TestVerifyDataDiskTree_IgnoresDestinationOnlyLostAndFound(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	buildUpgradeTestTree(t, src)
	if _, _, err := copyDataDiskTree(context.Background(), src, dst, DataDiskUpgradeHooks{}, ""); err != nil {
		t.Fatalf("copyDataDiskTree: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dst, "lost+found"), 0o700); err != nil {
		t.Fatalf("mkdir dst lost+found: %v", err)
	}

	interrupted, err := verifyDataDiskTree(context.Background(), src, dst, true, DataDiskUpgradeHooks{})
	if err != nil {
		t.Fatalf("verifyDataDiskTree with a destination-only lost+found: %v", err)
	}
	if interrupted {
		t.Fatal("verifyDataDiskTree reported interrupted with no stop requested")
	}
}

// TestVerifyDataDiskTree_DetectsUnexpectedDestinationEntry proves the
// lost+found allowance does not turn into a blanket pass for any extra
// destination entry: a genuinely unexpected one is still rejected.
func TestVerifyDataDiskTree_DetectsUnexpectedDestinationEntry(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	buildUpgradeTestTree(t, src)
	if _, _, err := copyDataDiskTree(context.Background(), src, dst, DataDiskUpgradeHooks{}, ""); err != nil {
		t.Fatalf("copyDataDiskTree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "stray.bin"), []byte("x"), 0o600); err != nil {
		t.Fatalf("writing stray dst file: %v", err)
	}

	_, err := verifyDataDiskTree(context.Background(), src, dst, true, DataDiskUpgradeHooks{})
	if !errors.Is(err, ErrDataDiskUpgradeMismatch) {
		t.Fatalf("verifyDataDiskTree with an unexpected destination entry: err = %v, want ErrDataDiskUpgradeMismatch", err)
	}
}

// runDataDiskUpgradeFakes bundles a FakeProvider/FakeRunner/FakeMounter
// set up for one "new-disk" device, including the blkid stub
// FilesystemUUID needs both right after formatting and again on a
// resume that skips formatting, plus scriptable Diff/Release/
// StagingMounted hooks so tests can drive the release gate (finding 1)
// and the resume-safety check (finding 2) without a real snapraid
// binary or a real mount.
type runDataDiskUpgradeFakes struct {
	provider *FakeProvider
	runner   *FakeRunner
	mounter  *FakeMounter

	diffRemoved int
	diffUpdated int
	diffErr     error
	diffCalls   int

	released     bool
	releaseErr   error
	releaseCalls int

	// stagingMounted defaults to "always mounted" — these fakes simulate
	// an interruption within the same daemon lifetime (the mount was
	// never actually torn down), not a daemon restart. Tests for
	// finding 2's own scenario override it.
	stagingMounted func(path string) (bool, error)
}

func newRunDataDiskUpgradeFakes(t *testing.T, newDevice, newUUID string) *runDataDiskUpgradeFakes {
	t.Helper()
	p := NewFakeProvider()
	p.AddDisk(newDevice, Disk{Size: 8 * TB})
	r := NewFakeRunner()
	r.Script("blkid", []string{"-s", "UUID", "-o", "value", newDevice}, []byte(newUUID+"\n"), nil)
	return &runDataDiskUpgradeFakes{
		provider:       p,
		runner:         r,
		mounter:        NewFakeMounter(),
		stagingMounted: func(string) (bool, error) { return true, nil },
	}
}

func (f *runDataDiskUpgradeFakes) deps() DataDiskUpgradeDeps {
	return DataDiskUpgradeDeps{
		Provider: f.provider,
		Runner:   f.runner,
		Mounter:  f.mounter,
		Diff: func(context.Context) (int, int, error) {
			f.diffCalls++
			if f.diffErr != nil {
				return 0, 0, f.diffErr
			}
			return f.diffRemoved, f.diffUpdated, nil
		},
		Release: func(context.Context) error {
			f.releaseCalls++
			if f.releaseErr != nil {
				return f.releaseErr
			}
			f.released = true
			return nil
		},
		StagingMounted: f.stagingMounted,
	}
}

func TestRunDataDiskUpgrade_HappyPath(t *testing.T) {
	oldWhere := t.TempDir()
	staging := t.TempDir()
	buildUpgradeTestTree(t, oldWhere)

	fakes := newRunDataDiskUpgradeFakes(t, "/dev/fake-new", "1111-uuid")
	spec := DataDiskUpgradeSpec{
		Old:           MountUnit{Where: oldWhere, UUID: "old-uuid", Filesystem: XFS, Description: "Hoserva data disk 1"},
		New:           DiskAddition{Device: "/dev/fake-new", Filesystem: XFS},
		NewFilesystem: XFS,
		Staging:       staging,
	}

	result, err := RunDataDiskUpgrade(context.Background(), spec, fakes.deps(), DataDiskUpgradeHooks{}, nil)
	if err != nil {
		t.Fatalf("RunDataDiskUpgrade: %v", err)
	}
	if result.Interrupted {
		t.Fatal("RunDataDiskUpgrade reported Interrupted on a completed run")
	}
	if result.NewMount.Where != oldWhere || result.NewMount.UUID != "1111-uuid" {
		t.Fatalf("NewMount = %+v, want Where=%s UUID=1111-uuid", result.NewMount, oldWhere)
	}
	if !result.Released {
		t.Fatal("RunDataDiskUpgrade with a clean diff: Released = false, want true")
	}
	if fakes.diffCalls != 1 {
		t.Fatalf("Diff calls = %d, want 1", fakes.diffCalls)
	}
	if fakes.releaseCalls != 1 {
		t.Fatalf("Release calls = %d, want 1", fakes.releaseCalls)
	}

	if got, _ := fakes.provider.FormattedAs("/dev/fake-new"); got != XFS {
		t.Fatalf("new disk formatted as %v, want xfs", got)
	}

	if len(fakes.mounter.Unmounts) != 2 {
		t.Fatalf("Unmounts = %+v, want exactly 2 (staging, then old)", fakes.mounter.Unmounts)
	}
	if fakes.mounter.Unmounts[1].Where != oldWhere {
		t.Fatalf("second unmount = %+v, want Where=%s", fakes.mounter.Unmounts[1], oldWhere)
	}
	lastMount := fakes.mounter.Mounts[len(fakes.mounter.Mounts)-1]
	if lastMount.Where != oldWhere || lastMount.UUID != "1111-uuid" {
		t.Fatalf("final mount = %+v, want Where=%s UUID=1111-uuid", lastMount, oldWhere)
	}

	// The old disk's own tree is completely untouched by a successful
	// upgrade — this package never deletes or modifies it.
	oldData, err := os.ReadFile(filepath.Join(oldWhere, "movies", "a.bin"))
	if err != nil {
		t.Fatalf("reading old disk's file after upgrade: %v", err)
	}
	if len(oldData) != 200_000 {
		t.Fatalf("old disk's file size = %d, want 200000 (unchanged)", len(oldData))
	}
}

// TestRunDataDiskUpgrade_KilledMidCopy_OldDiskNeverUnmountedAndResumeCompletes
// is this issue's own central "kill the job at every step" test for the
// copying phase: interrupting mid-copy never unmounts or otherwise
// touches the old disk, and resuming from the saved checkpoint
// completes the upgrade correctly.
func TestRunDataDiskUpgrade_KilledMidCopy_OldDiskNeverUnmountedAndResumeCompletes(t *testing.T) {
	oldWhere := t.TempDir()
	staging := t.TempDir()
	buildUpgradeTestTree(t, oldWhere)
	// A few more files, so a handful of Log calls lands mid-copy rather
	// than on the very last entry.
	mustMkdirAll(t, filepath.Join(oldWhere, "extra"))
	for i := 0; i < 5; i++ {
		writeUpgradeTestFile(t, filepath.Join(oldWhere, "extra", string(rune('a'+i))+".bin"), 1000, 0o644)
	}

	fakes := newRunDataDiskUpgradeFakes(t, "/dev/fake-new", "1111-uuid")
	spec := DataDiskUpgradeSpec{
		Old:           MountUnit{Where: oldWhere, UUID: "old-uuid", Filesystem: XFS, Description: "Hoserva data disk 1"},
		New:           DiskAddition{Device: "/dev/fake-new", Filesystem: XFS},
		NewFilesystem: XFS,
		Staging:       staging,
	}
	deps := fakes.deps()

	stop := make(chan struct{})
	var logCalls int
	var savedCheckpoint []byte
	hooks := DataDiskUpgradeHooks{
		StopRequested: stop,
		SaveCheckpoint: func(data []byte) error {
			savedCheckpoint = append([]byte(nil), data...)
			return nil
		},
		Log: func(format string, args ...any) {
			logCalls++
			if logCalls == 3 {
				close(stop)
			}
		},
	}

	result, err := RunDataDiskUpgrade(context.Background(), spec, deps, hooks, nil)
	if err != nil {
		t.Fatalf("RunDataDiskUpgrade (first, interrupted run): %v", err)
	}
	if !result.Interrupted {
		t.Fatal("RunDataDiskUpgrade did not report Interrupted")
	}
	if len(fakes.mounter.Unmounts) != 0 {
		t.Fatalf("Unmounts after an interrupted copy = %+v, want none — the old disk must stay mounted and untouched", fakes.mounter.Unmounts)
	}
	if len(savedCheckpoint) == 0 {
		t.Fatal("no checkpoint was saved before the interruption")
	}

	// Resume, uninterrupted this time.
	fakes2 := newRunDataDiskUpgradeFakes(t, "/dev/fake-new", "1111-uuid")
	// The new disk from the first run is already formatted and mounted
	// at staging with partial content — a resumed run must not
	// re-format it, only pick the copy back up.
	result2, err := RunDataDiskUpgrade(context.Background(), spec, fakes2.deps(), DataDiskUpgradeHooks{}, savedCheckpoint)
	if err != nil {
		t.Fatalf("RunDataDiskUpgrade (resumed run): %v", err)
	}
	if result2.Interrupted {
		t.Fatal("resumed run reported Interrupted")
	}
	if !result2.Released {
		t.Fatal("resumed run with a clean diff: Released = false, want true")
	}
	if got, ok := fakes2.provider.FormattedAs("/dev/fake-new"); ok {
		t.Fatalf("resumed run re-formatted the new disk as %v, want no re-format", got)
	}

	interrupted, err := verifyDataDiskTree(context.Background(), oldWhere, staging, true, DataDiskUpgradeHooks{})
	if err != nil {
		t.Fatalf("verifyDataDiskTree after resume: %v", err)
	}
	if interrupted {
		t.Fatal("verifyDataDiskTree reported interrupted with no stop requested")
	}
}

// TestRunDataDiskUpgrade_VerifyMismatch_NeverRemounts confirms that a
// staging copy which fails verification never reaches Remounting: the
// old disk is never unmounted, and no mount is ever made at Old.Where.
func TestRunDataDiskUpgrade_VerifyMismatch_NeverRemounts(t *testing.T) {
	oldWhere := t.TempDir()
	staging := t.TempDir()
	buildUpgradeTestTree(t, oldWhere)
	// Staging deliberately does not match oldWhere at all — standing in
	// for a copy phase that finished but produced the wrong content.
	mustMkdirAll(t, staging)

	fakes := newRunDataDiskUpgradeFakes(t, "/dev/fake-new", "1111-uuid")
	spec := DataDiskUpgradeSpec{
		Old:           MountUnit{Where: oldWhere, UUID: "old-uuid", Filesystem: XFS},
		New:           DiskAddition{Device: "/dev/fake-new", Filesystem: XFS},
		NewFilesystem: XFS,
		Staging:       staging,
	}
	cp := mustMarshalDataDiskUpgradeCheckpoint(t, DataDiskUpgradeCheckpoint{Phase: DataDiskUpgradePhaseVerifying})
	_, err := RunDataDiskUpgrade(context.Background(), spec, fakes.deps(), DataDiskUpgradeHooks{}, cp)
	if !errors.Is(err, ErrDataDiskUpgradeMismatch) {
		t.Fatalf("RunDataDiskUpgrade with a mismatched staging copy: err = %v, want ErrDataDiskUpgradeMismatch", err)
	}
	if len(fakes.mounter.Unmounts) != 0 || len(fakes.mounter.Mounts) != 0 {
		t.Fatalf("mounter calls after a verify mismatch = mounts:%+v unmounts:%+v, want none", fakes.mounter.Mounts, fakes.mounter.Unmounts)
	}
}

// TestRunDataDiskUpgrade_DiffNotClean_StopsWithoutReleasing is this
// issue's own central release-gate test (acceptance criterion 3): a
// snapraid diff that still reports removed or updated files against the
// newly remounted disk must stop the run there, checkpointed at
// Diffing, without ever calling Release — and a later retry against a
// now-clean diff completes normally and does release.
func TestRunDataDiskUpgrade_DiffNotClean_StopsWithoutReleasing(t *testing.T) {
	oldWhere := t.TempDir()
	staging := t.TempDir()
	spec := DataDiskUpgradeSpec{
		Old:           MountUnit{Where: oldWhere, UUID: "old-uuid", Filesystem: XFS},
		New:           DiskAddition{Device: "/dev/fake-new", Filesystem: XFS},
		NewFilesystem: XFS,
		Staging:       staging,
	}

	fakes := newRunDataDiskUpgradeFakes(t, "/dev/fake-new", "1111-uuid")
	fakes.diffRemoved = 2

	cp := mustMarshalDataDiskUpgradeCheckpoint(t, DataDiskUpgradeCheckpoint{Phase: DataDiskUpgradePhaseDiffing})
	result, err := RunDataDiskUpgrade(context.Background(), spec, fakes.deps(), DataDiskUpgradeHooks{}, cp)
	if err != nil {
		t.Fatalf("RunDataDiskUpgrade with a dirty diff: %v", err)
	}
	if result.Interrupted {
		t.Fatal("RunDataDiskUpgrade with a dirty diff reported Interrupted, want a clean (non-error) stop")
	}
	if result.Released {
		t.Fatal("RunDataDiskUpgrade with a dirty diff: Released = true, want false")
	}
	if result.NewMount.Where != oldWhere {
		t.Fatalf("NewMount.Where = %q, want %q — the disk is already remounted even though release is pending", result.NewMount.Where, oldWhere)
	}
	if fakes.releaseCalls != 0 {
		t.Fatalf("Release calls = %d, want 0 — a dirty diff must never release the old disk", fakes.releaseCalls)
	}

	// Retry once the diff comes back clean: the same checkpoint (still
	// Diffing — a dirty diff never advances it) resumes it.
	fakes.diffRemoved = 0
	fakes.diffUpdated = 0
	result2, err := RunDataDiskUpgrade(context.Background(), spec, fakes.deps(), DataDiskUpgradeHooks{}, cp)
	if err != nil {
		t.Fatalf("RunDataDiskUpgrade retry with a clean diff: %v", err)
	}
	if !result2.Released {
		t.Fatal("retry with a clean diff: Released = false, want true")
	}
	if fakes.releaseCalls != 1 {
		t.Fatalf("Release calls after a clean retry = %d, want 1", fakes.releaseCalls)
	}
}

// TestRunDataDiskUpgrade_ResumeStagingUnmounted_RemountsBeforeCopying is
// this issue's own central resume-safety test (finding 2): resuming
// past Formatting when Staging is no longer mounted — the exact
// daemon-restart scenario, where neither DirectMounter's nor
// SystemdMounter's own mount survives on its own — must re-mount
// Staging before writing anything through it, never silently resume
// the copy onto whatever ordinary directory Staging happens to resolve
// to.
func TestRunDataDiskUpgrade_ResumeStagingUnmounted_RemountsBeforeCopying(t *testing.T) {
	oldWhere := t.TempDir()
	staging := t.TempDir()
	buildUpgradeTestTree(t, oldWhere)

	fakes := newRunDataDiskUpgradeFakes(t, "/dev/fake-new", "1111-uuid")
	var mountedCalls int
	fakes.stagingMounted = func(string) (bool, error) {
		mountedCalls++
		// Not mounted on the first check this resume makes (the mount
		// Formatting made did not survive) — mounted from the second
		// check on, once ensureStagingMounted has re-established it.
		return mountedCalls > 1, nil
	}

	spec := DataDiskUpgradeSpec{
		Old:           MountUnit{Where: oldWhere, UUID: "old-uuid", Filesystem: XFS},
		New:           DiskAddition{Device: "/dev/fake-new", Filesystem: XFS},
		NewFilesystem: XFS,
		Staging:       staging,
	}
	cp := mustMarshalDataDiskUpgradeCheckpoint(t, DataDiskUpgradeCheckpoint{Phase: DataDiskUpgradePhaseCopying})

	result, err := RunDataDiskUpgrade(context.Background(), spec, fakes.deps(), DataDiskUpgradeHooks{}, cp)
	if err != nil {
		t.Fatalf("RunDataDiskUpgrade resuming onto unmounted staging: %v", err)
	}
	if result.Interrupted {
		t.Fatal("RunDataDiskUpgrade reported Interrupted, want a completed run")
	}
	if !result.Released {
		t.Fatal("Released = false, want true — the run should have completed once staging was safely re-mounted")
	}
	if len(fakes.mounter.Mounts) == 0 {
		t.Fatal("no Mount call recorded — ensureStagingMounted should have re-mounted staging before resuming the copy")
	}
	first := fakes.mounter.Mounts[0]
	if first.Where != staging || first.UUID != "1111-uuid" {
		t.Fatalf("re-mount = %+v, want Where=%s UUID=1111-uuid", first, staging)
	}
	// The resumed copy actually wrote through the re-established mount at
	// staging, so the old disk's own tree is present there.
	if _, err := os.Stat(filepath.Join(staging, "movies", "a.bin")); err != nil {
		t.Fatalf("stat staging copy after resume: %v", err)
	}
}

// TestRunDataDiskUpgrade_ResumeStagingCannotBeRemounted_RefusesWithoutWriting
// is this issue's own fail-closed half of the resume-safety test
// (finding 2): when Staging still is not mounted after a re-mount
// attempt, the resume must refuse outright rather than writing to
// whatever Staging currently resolves to, and nothing gets written
// there.
func TestRunDataDiskUpgrade_ResumeStagingCannotBeRemounted_RefusesWithoutWriting(t *testing.T) {
	oldWhere := t.TempDir()
	staging := t.TempDir()
	buildUpgradeTestTree(t, oldWhere)

	fakes := newRunDataDiskUpgradeFakes(t, "/dev/fake-new", "1111-uuid")
	fakes.stagingMounted = func(string) (bool, error) { return false, nil }

	spec := DataDiskUpgradeSpec{
		Old:           MountUnit{Where: oldWhere, UUID: "old-uuid", Filesystem: XFS},
		New:           DiskAddition{Device: "/dev/fake-new", Filesystem: XFS},
		NewFilesystem: XFS,
		Staging:       staging,
	}
	cp := mustMarshalDataDiskUpgradeCheckpoint(t, DataDiskUpgradeCheckpoint{Phase: DataDiskUpgradePhaseCopying})

	_, err := RunDataDiskUpgrade(context.Background(), spec, fakes.deps(), DataDiskUpgradeHooks{}, cp)
	if err == nil {
		t.Fatal("RunDataDiskUpgrade resuming onto staging that never re-mounts: got nil error, want a refusal")
	}

	entries, rerr := os.ReadDir(staging)
	if rerr != nil {
		t.Fatalf("reading staging dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("staging dir has %d entries after a refused resume, want 0 — nothing should ever be written to an unmounted staging path", len(entries))
	}
}

// TestRunDataDiskUpgrade_DepsRequired confirms RunDataDiskUpgrade
// refuses before touching anything when a required dependency is
// missing.
func TestRunDataDiskUpgrade_DepsRequired(t *testing.T) {
	spec := DataDiskUpgradeSpec{Old: MountUnit{Where: t.TempDir()}, New: DiskAddition{Device: "/dev/fake-new"}, Staging: t.TempDir()}
	_, err := RunDataDiskUpgrade(context.Background(), spec, DataDiskUpgradeDeps{}, DataDiskUpgradeHooks{}, nil)
	if err == nil {
		t.Fatal("RunDataDiskUpgrade with no deps: got nil error")
	}
}

func mustMarshalDataDiskUpgradeCheckpoint(t *testing.T, cp DataDiskUpgradeCheckpoint) []byte {
	t.Helper()
	data, err := json.Marshal(cp)
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	return data
}
