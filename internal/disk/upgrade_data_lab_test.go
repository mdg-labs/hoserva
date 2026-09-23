//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45),
// never on the host: it is built with `go test -tags lab -c` from the
// host (compiling touches no device) and the resulting binary is run
// with `docker compose exec -T lab <binary>` inside the lab container,
// the same pattern format_lab_test.go and lifecycle_lab_test.go use. It
// exercises this issue's own "Larger data disk" acceptance criteria
// (doc 02 §4, Q71) against real loop devices and a real snapraid
// 12.4-1 binary: copying with ownership and timestamps preserved,
// remounting the new disk at the old disk's own mountpoint, and
// requiring a real `snapraid diff` against the unchanged configuration
// to show nothing removed or updated — the exact expectation Q71
// requires confirming in the lab before this ships. It also proves
// that killing the job mid-copy never touches the old disk, and that a
// separate, uninvolved data disk failing mid-upgrade still reconstructs
// normally via `snapraid fix`.
//
// Every disk this file touches is its own, dedicated loop device —
// never disk1-3 of `make lab-up`'s own standing array — for the same
// reason internal/parity's own lifecycle_lab_test.go gives: this file's
// own upgrade genuinely unmounts and remounts a data disk, which would
// corrupt whichever other lab test file in this package runs
// afterward if it touched shared state.

package disk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// createAttachedLoopDevice truncates a fresh sparse image of sizeMB
// under lab's own img/ directory and attaches it, unformatted — the
// device RunDataDiskUpgrade's own Formatting phase is meant to format,
// so this helper never runs mkfs itself (unlike
// createFormattedMountedDataDisk below).
func createAttachedLoopDevice(t *testing.T, lab, name, sizeMB string) string {
	t.Helper()
	imgDir := filepath.Join(lab, "img")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", imgDir, err)
	}
	img := filepath.Join(imgDir, name+".img")
	if err := os.Remove(img); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing stale %s: %v", img, err)
	}
	if out, err := exec.Command("truncate", "-s", sizeMB, img).CombinedOutput(); err != nil {
		t.Fatalf("truncate %s: %v: %s", img, err, out)
	}
	out, err := exec.Command("losetup", "--find", "--show", img).Output()
	if err != nil {
		t.Fatalf("losetup --find --show %s: %v", img, err)
	}
	dev := strings.TrimSpace(string(out))
	assertOwnLoopDataUpgrade(t, dev, img)
	t.Cleanup(func() {
		_, _ = exec.Command("losetup", "-d", dev).CombinedOutput()
	})
	return dev
}

// createFormattedMountedDataDisk is createAttachedLoopDevice plus a
// real mkfs.xfs and mount at mountpoint — an "old" disk already in
// service, the state RunDataDiskUpgrade always finds Old.Where in
// before an upgrade ever starts.
func createFormattedMountedDataDisk(t *testing.T, lab, name, mountpoint, sizeMB string) string {
	t.Helper()
	dev := createAttachedLoopDevice(t, lab, name, sizeMB)
	if out, err := exec.Command("mkfs.xfs", "-q", dev).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.xfs %s: %v: %s", dev, err, out)
	}
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", mountpoint, err)
	}
	if out, err := exec.Command("mount", dev, mountpoint).CombinedOutput(); err != nil {
		t.Fatalf("mount %s %s: %v: %s", dev, mountpoint, err, out)
	}
	t.Cleanup(func() {
		_, _ = exec.Command("umount", mountpoint).CombinedOutput()
	})
	return dev
}

func assertOwnLoopDataUpgrade(t *testing.T, dev, img string) {
	t.Helper()
	if !strings.HasPrefix(dev, "/dev/loop") {
		t.Fatalf("refusing non-loop device: %s", dev)
	}
	out, err := exec.Command("losetup", "-j", img, "--output", "NAME", "--noheadings").Output()
	if err != nil {
		t.Fatalf("losetup -j %s: %v", img, err)
	}
	if resolved := strings.TrimSpace(string(out)); resolved != dev {
		t.Fatalf("refusing %s: not backed by %s (losetup -j reports %q)", dev, img, resolved)
	}
}

// findmntSource returns the device currently mounted at where.
func findmntSource(t *testing.T, where string) string {
	t.Helper()
	out, err := exec.Command("findmnt", "-n", "-o", "SOURCE", "--target", where).Output()
	if err != nil {
		t.Fatalf("findmnt --target %s: %v", where, err)
	}
	return strings.TrimSpace(string(out))
}

func writeRandomLabFile(t *testing.T, path string, size int) []byte {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return data
}

func sha256HexLab(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// dataUpgradeLabEngine builds a real, self-contained snapraid.conf over
// one dedicated parity disk and dataMount as the sole data disk — never
// the standing array's own parity1/disk1-3 — so this file's own upgrade
// (which genuinely unmounts and remounts dataMount) can never disturb,
// or be disturbed by, any other lab test in this package or in
// internal/parity. contentMount hosts this fileset's second content
// copy — real snapraid refuses a configuration with fewer than two
// content files on different disks, confirmed against 12.4-1 in this
// lab — kept off both parityMount and dataMount so neither the parity
// disk swap this file never performs nor the data disk swap it does
// perform ever has to account for a content file living on the disk
// being changed.
func dataUpgradeLabEngine(t *testing.T, confPath, filesetName, parityMount, contentMount, dataMount string) *parity.SnapraidEngine {
	t.Helper()
	conf := fmt.Sprintf(
		"parity %s\ncontent %s\ncontent %s\ndata d1 %s/\n",
		filepath.Join(parityMount, filesetName+".parity"),
		filepath.Join(parityMount, filesetName+".content"),
		filepath.Join(contentMount, filesetName+".content"),
		dataMount,
	)
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatalf("writing %s: %v", confPath, err)
	}
	return &parity.SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(filepath.Dir(confPath), "logs"),
		Runner:   parity.CommandRunner{},
		// This lab's own arrays are tiny by construction (Q45's loop
		// devices), so a handful of scripted file changes routinely
		// exceeds the guard's production thresholds for reasons
		// unrelated to what these tests exercise — the same rationale
		// internal/parity's own lab tests give for the same override.
		Guard: parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}
}

func syncOnceDataUpgrade(t *testing.T, ctx context.Context, engine *parity.SnapraidEngine) {
	t.Helper()
	ch, err := engine.Sync(ctx, parity.SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	var final parity.Progress
	for p := range ch {
		final = p
	}
	if final.Err != nil {
		t.Fatalf("Sync failed: %v", final.Err)
	}
}

// TestLabDataDiskUpgrade_CopiesVerifiesRemountsWithCleanRealDiff is this
// issue's own central "Larger data disk" happy path: a real disk's
// files, synced into a real array, are copied onto a larger real disk
// preserving ownership and timestamps, the new disk is mounted at the
// exact same mountpoint the old one used, and — without changing
// snapraid.conf at all, since the mount path is unchanged — a real
// `snapraid diff` against the unchanged configuration reports nothing
// removed or updated (Q71's own lab-confirmation requirement, against
// snapraid 12.4-1).
func TestLabDataDiskUpgrade_CopiesVerifiesRemountsWithCleanRealDiff(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	provider := &LinuxProvider{Lister: NewLister(), Exec: r}
	mounter := DirectMounter{Runner: r}

	oldWhere := filepath.Join(lab, "mnt", "p116-data-old")
	createFormattedMountedDataDisk(t, lab, "p116-data-old", oldWhere, "330M")
	parityMount := filepath.Join(lab, "mnt", "p116-data-parity")
	createFormattedMountedDataDisk(t, lab, "p116-data-parity", parityMount, "330M")
	contentMount := filepath.Join(lab, "mnt", "p116-data-content2")
	createFormattedMountedDataDisk(t, lab, "p116-data-content2", contentMount, "310M")
	newDev := createAttachedLoopDevice(t, lab, "p116-data-new", "360M")
	staging := filepath.Join(lab, "mnt", "p116-data-staging")

	// Real content: a nested directory, a symlink, and a file with a
	// deliberately distinctive, non-"now" mtime and a non-default mode —
	// exactly what a copy that merely inherited "now" rather than
	// actually preserving the source's own metadata would fail to
	// reproduce.
	original := writeRandomLabFile(t, filepath.Join(oldWhere, "movies", "keepsake.bin"), 400_000)
	origHash := sha256HexLab(original)
	if err := os.Chmod(filepath.Join(oldWhere, "movies", "keepsake.bin"), 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	mtime := time.Date(2021, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(oldWhere, "movies", "keepsake.bin"), mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Symlink("keepsake.bin", filepath.Join(oldWhere, "movies", "keepsake-link.bin")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	confPath := filepath.Join(lab, "p116-data-upgrade", "snapraid.conf")
	if err := os.MkdirAll(filepath.Dir(confPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	engine := dataUpgradeLabEngine(t, confPath, "p116-data-upgrade", parityMount, contentMount, oldWhere)
	syncOnceDataUpgrade(t, ctx, engine)

	beforeDiff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff before upgrade: %v", err)
	}
	if beforeDiff.Added != 0 || beforeDiff.Removed != 0 || beforeDiff.Updated != 0 {
		t.Fatalf("Diff before upgrade reported pending changes: %+v, want none", beforeDiff)
	}

	oldUUID, err := FilesystemUUID(ctx, r, findmntSource(t, oldWhere))
	if err != nil {
		t.Fatalf("FilesystemUUID (old): %v", err)
	}

	// Diff and Release are wired to the real engine and a tracked flag,
	// so this issue's own central lab-confirmation requirement — a real
	// `snapraid diff` gating a real release — is exercised through
	// RunDataDiskUpgrade itself, not asserted only from outside it.
	var released bool
	spec := DataDiskUpgradeSpec{
		Old:           MountUnit{Where: oldWhere, UUID: oldUUID, Filesystem: XFS},
		New:           DiskAddition{Device: newDev, Filesystem: XFS},
		NewFilesystem: XFS,
		Staging:       staging,
	}
	deps := DataDiskUpgradeDeps{
		Provider: provider,
		Runner:   r,
		Mounter:  mounter,
		ConfirmMounted: func(ctx context.Context, where, uuid string) error {
			return ConfirmMountedUUID(ctx, r, where, uuid)
		},
		Diff: func(ctx context.Context) (int, int, error) {
			d, err := engine.Diff(ctx)
			if err != nil {
				return 0, 0, err
			}
			return d.Removed, d.Updated, nil
		},
		Release: func(context.Context) error {
			released = true
			return nil
		},
	}

	result, err := RunDataDiskUpgrade(ctx, spec, deps, DataDiskUpgradeHooks{}, nil)
	if err != nil {
		t.Fatalf("RunDataDiskUpgrade: %v", err)
	}
	if result.Interrupted {
		t.Fatal("RunDataDiskUpgrade reported Interrupted on a completed run")
	}
	if result.NewMount.Where != oldWhere {
		t.Fatalf("NewMount.Where = %q, want %q — the new disk must mount at the old disk's own mountpoint", result.NewMount.Where, oldWhere)
	}
	// This issue's own central lab-confirmation requirement: against a
	// real snapraid 12.4-1 binary and an unchanged snapraid.conf (same
	// mount path), the real `snapraid diff` RunDataDiskUpgrade itself ran
	// came back clean, and it released the old disk as a result.
	if !result.Released {
		t.Fatal("RunDataDiskUpgrade: Released = false, want true — a real snapraid diff against an unchanged config should be clean")
	}
	if !released {
		t.Fatal("Release was never called after a successful upgrade with a clean diff")
	}

	if got := findmntSource(t, oldWhere); got != newDev {
		t.Fatalf("device now mounted at %s = %q, want the new disk %q", oldWhere, got, newDev)
	}

	// Ownership, mode and timestamps, confirmed against the real
	// filesystem rather than through this package's own copy/verify
	// helpers.
	info, err := os.Stat(filepath.Join(oldWhere, "movies", "keepsake.bin"))
	if err != nil {
		t.Fatalf("stat after remount: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode after remount = %v, want 0640", info.Mode().Perm())
	}
	if !info.ModTime().Equal(mtime) {
		t.Fatalf("mtime after remount = %v, want %v", info.ModTime(), mtime)
	}
	restored, err := os.ReadFile(filepath.Join(oldWhere, "movies", "keepsake.bin"))
	if err != nil {
		t.Fatalf("reading file after remount: %v", err)
	}
	if sha256HexLab(restored) != origHash {
		t.Fatal("file content changed across the upgrade")
	}
	if target, err := os.Readlink(filepath.Join(oldWhere, "movies", "keepsake-link.bin")); err != nil || target != "keepsake.bin" {
		t.Fatalf("symlink after remount: target=%q err=%v, want keepsake.bin", target, err)
	}

	// A further write onto the new disk, synced normally, confirms it
	// is genuinely live and protected, not merely a copy nobody uses.
	writeRandomLabFile(t, filepath.Join(oldWhere, "movies", "after-upgrade.bin"), 50_000)
	syncOnceDataUpgrade(t, ctx, engine)
	finalDiff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after a post-upgrade sync: %v", err)
	}
	if finalDiff.Added != 0 || finalDiff.Removed != 0 || finalDiff.Updated != 0 {
		t.Fatalf("Diff after a post-upgrade sync reported changes: %+v, want none", finalDiff)
	}
}

// TestLabDataDiskUpgrade_KilledMidCopy_OldDiskStaysMountedAndValidThenResumeCompletes
// is this issue's own central "kill the job at every step" test for the
// copying phase against real devices: interrupting mid-copy leaves the
// old disk mounted at its own path with every original file intact —
// Remounting never having run — and resuming from the saved checkpoint
// finishes the upgrade correctly.
func TestLabDataDiskUpgrade_KilledMidCopy_OldDiskStaysMountedAndValidThenResumeCompletes(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	provider := &LinuxProvider{Lister: NewLister(), Exec: r}
	mounter := DirectMounter{Runner: r}

	oldWhere := filepath.Join(lab, "mnt", "p116-data-kill-old")
	oldDev := createFormattedMountedDataDisk(t, lab, "p116-data-kill-old", oldWhere, "330M")
	newDev := createAttachedLoopDevice(t, lab, "p116-data-kill-new", "360M")
	staging := filepath.Join(lab, "mnt", "p116-data-kill-staging")

	var originals [][]byte
	for i := 0; i < 5; i++ {
		originals = append(originals, writeRandomLabFile(t, filepath.Join(oldWhere, "movies", fmt.Sprintf("f%d.bin", i)), 300_000))
	}

	oldUUID, err := FilesystemUUID(ctx, r, oldDev)
	if err != nil {
		t.Fatalf("FilesystemUUID (old): %v", err)
	}
	spec := DataDiskUpgradeSpec{
		Old:           MountUnit{Where: oldWhere, UUID: oldUUID, Filesystem: XFS},
		New:           DiskAddition{Device: newDev, Filesystem: XFS},
		NewFilesystem: XFS,
		Staging:       staging,
	}
	// No snapraid.conf exists in this test — it exercises the copy/
	// remount resume path only, not the release gate — so Diff/Release
	// are no-op stubs, just enough to satisfy DataDiskUpgradeDeps' own
	// required-fields check.
	var released bool
	deps := DataDiskUpgradeDeps{
		Provider: provider,
		Runner:   r,
		Mounter:  mounter,
		ConfirmMounted: func(ctx context.Context, where, uuid string) error {
			return ConfirmMountedUUID(ctx, r, where, uuid)
		},
		Diff:    func(context.Context) (int, int, error) { return 0, 0, nil },
		Release: func(context.Context) error { released = true; return nil },
	}

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

	result, err := RunDataDiskUpgrade(ctx, spec, deps, hooks, nil)
	if err != nil {
		t.Fatalf("RunDataDiskUpgrade (first, interrupted run): %v", err)
	}
	if !result.Interrupted {
		t.Fatal("RunDataDiskUpgrade did not report Interrupted mid-copy")
	}

	// The old disk is still exactly the device mounted at its own path —
	// Remounting never ran — and every original file is untouched.
	if got := findmntSource(t, oldWhere); got != oldDev {
		t.Fatalf("device mounted at %s after interruption = %q, want the old disk %q unchanged", oldWhere, got, oldDev)
	}
	for i, orig := range originals {
		got, err := os.ReadFile(filepath.Join(oldWhere, "movies", fmt.Sprintf("f%d.bin", i)))
		if err != nil {
			t.Fatalf("reading old disk's file %d after interruption: %v", i, err)
		}
		if string(got) != string(orig) {
			t.Fatalf("old disk's file %d changed after an interrupted copy", i)
		}
	}
	if len(savedCheckpoint) == 0 {
		t.Fatal("no checkpoint was saved before the interruption")
	}

	result2, err := RunDataDiskUpgrade(ctx, spec, deps, DataDiskUpgradeHooks{}, savedCheckpoint)
	if err != nil {
		t.Fatalf("RunDataDiskUpgrade (resumed run): %v", err)
	}
	if result2.Interrupted {
		t.Fatal("resumed run reported Interrupted")
	}
	if !result2.Released {
		t.Fatal("resumed run: Released = false, want true (a stubbed, always-clean diff)")
	}
	if !released {
		t.Fatal("Release was never called after the resumed run completed")
	}
	if got := findmntSource(t, oldWhere); got != newDev {
		t.Fatalf("device mounted at %s after the resumed run = %q, want the new disk %q", oldWhere, got, newDev)
	}
	for i, orig := range originals {
		got, err := os.ReadFile(filepath.Join(oldWhere, "movies", fmt.Sprintf("f%d.bin", i)))
		if err != nil {
			t.Fatalf("reading file %d after the resumed upgrade: %v", i, err)
		}
		if string(got) != string(orig) {
			t.Fatalf("file %d does not match its original content after the resumed upgrade", i)
		}
	}
}

// TestLabDataDiskUpgrade_ResumeAfterRestartStagingUnmounted_RemountsAndResumesSafely
// is this issue's own resume-safety test (finding 2) against a real
// mount, not a fake: interrupting the copy, then genuinely unmounting
// Staging by hand — the same real effect a daemon restart has, since
// neither DirectMounter's `mount` nor SystemdMounter's `systemctl
// start` leaves a mount in place on its own — and confirming the
// resumed run re-mounts the same partially-copied filesystem (rather
// than silently writing onto an ordinary directory on the root
// filesystem) before it continues, finishing with every file correct.
func TestLabDataDiskUpgrade_ResumeAfterRestartStagingUnmounted_RemountsAndResumesSafely(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	provider := &LinuxProvider{Lister: NewLister(), Exec: r}
	mounter := DirectMounter{Runner: r}

	oldWhere := filepath.Join(lab, "mnt", "p116-data-restart-old")
	oldDev := createFormattedMountedDataDisk(t, lab, "p116-data-restart-old", oldWhere, "330M")
	newDev := createAttachedLoopDevice(t, lab, "p116-data-restart-new", "360M")
	staging := filepath.Join(lab, "mnt", "p116-data-restart-staging")

	var originals [][]byte
	for i := 0; i < 5; i++ {
		originals = append(originals, writeRandomLabFile(t, filepath.Join(oldWhere, "movies", fmt.Sprintf("f%d.bin", i)), 300_000))
	}

	oldUUID, err := FilesystemUUID(ctx, r, oldDev)
	if err != nil {
		t.Fatalf("FilesystemUUID (old): %v", err)
	}
	spec := DataDiskUpgradeSpec{
		Old:           MountUnit{Where: oldWhere, UUID: oldUUID, Filesystem: XFS},
		New:           DiskAddition{Device: newDev, Filesystem: XFS},
		NewFilesystem: XFS,
		Staging:       staging,
	}
	deps := DataDiskUpgradeDeps{
		Provider: provider,
		Runner:   r,
		Mounter:  mounter,
		ConfirmMounted: func(ctx context.Context, where, uuid string) error {
			return ConfirmMountedUUID(ctx, r, where, uuid)
		},
		Diff:    func(context.Context) (int, int, error) { return 0, 0, nil },
		Release: func(context.Context) error { return nil },
	}

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

	result, err := RunDataDiskUpgrade(ctx, spec, deps, hooks, nil)
	if err != nil {
		t.Fatalf("RunDataDiskUpgrade (first, interrupted run): %v", err)
	}
	if !result.Interrupted {
		t.Fatal("RunDataDiskUpgrade did not report Interrupted mid-copy")
	}

	// The real effect of a daemon restart: Staging's mount is gone,
	// though the new disk's own filesystem — with whatever partial copy
	// it already has — is untouched, still attached at newDev.
	if out, err := exec.Command("umount", staging).CombinedOutput(); err != nil {
		t.Fatalf("umount %s: %v: %s", staging, err, out)
	}
	if mounted, err := IsMountpoint(staging); err != nil {
		t.Fatalf("IsMountpoint(%s): %v", staging, err)
	} else if mounted {
		t.Fatalf("%s is still mounted after umount", staging)
	}

	resumeResult, err := RunDataDiskUpgrade(ctx, spec, deps, DataDiskUpgradeHooks{}, savedCheckpoint)
	if err != nil {
		t.Fatalf("RunDataDiskUpgrade (resumed run onto unmounted staging): %v", err)
	}
	if resumeResult.Interrupted {
		t.Fatal("resumed run reported Interrupted")
	}
	if got := findmntSource(t, oldWhere); got != newDev {
		t.Fatalf("device mounted at %s after the resumed run = %q, want the new disk %q", oldWhere, got, newDev)
	}
	for i, orig := range originals {
		got, err := os.ReadFile(filepath.Join(oldWhere, "movies", fmt.Sprintf("f%d.bin", i)))
		if err != nil {
			t.Fatalf("reading file %d after the resumed upgrade: %v", i, err)
		}
		if string(got) != string(orig) {
			t.Fatalf("file %d does not match its original content after the resumed upgrade — the resumed copy must have written through the re-established mount, not onto a stray directory", i)
		}
	}
}

// TestLabDataDiskUpgrade_FailDifferentDiskMidUpgrade_ReconstructsViaFix
// is this issue's own "fail a different data disk mid-upgrade and
// reconstruct it" acceptance criterion: while disk d1 is mid-upgrade, a
// wholly separate, uninvolved data disk (d2) genuinely fails — detached
// and replaced with a fresh device at the same mountpoint, doc 06 §3's
// own "kill a disk mid-operation" recipe — and `snapraid fix -d d2`
// still fully reconstructs its pre-failure contents, proving the
// in-progress upgrade of d1 never left parity in a state that breaks
// recovery of an unrelated disk.
func TestLabDataDiskUpgrade_FailDifferentDiskMidUpgrade_ReconstructsViaFix(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	provider := &LinuxProvider{Lister: NewLister(), Exec: r}
	mounter := DirectMounter{Runner: r}

	upgradingWhere := filepath.Join(lab, "mnt", "p116-data-victim-upgrading")
	upgradingDev := createFormattedMountedDataDisk(t, lab, "p116-data-victim-upgrading", upgradingWhere, "330M")
	victimWhere := filepath.Join(lab, "mnt", "p116-data-victim-uninvolved")
	createFormattedMountedDataDisk(t, lab, "p116-data-victim-uninvolved", victimWhere, "330M")
	parityMount := filepath.Join(lab, "mnt", "p116-data-victim-parity")
	createFormattedMountedDataDisk(t, lab, "p116-data-victim-parity", parityMount, "330M")
	newDev := createAttachedLoopDevice(t, lab, "p116-data-victim-new", "360M")
	staging := filepath.Join(lab, "mnt", "p116-data-victim-staging")
	// This test leaves its upgrade interrupted with staging mounted; a
	// rerun in the same lab would otherwise stack a second mount there.
	t.Cleanup(func() {
		_, _ = exec.Command("umount", staging).CombinedOutput()
	})

	confPath := filepath.Join(lab, "p116-data-victim", "snapraid.conf")
	if err := os.MkdirAll(filepath.Dir(confPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The second content copy lives on victimWhere itself (a data disk,
	// same as labEngine's own lab convention) rather than on
	// upgradingWhere — real snapraid refuses fewer than two content
	// files on different disks, and a content file must never live on
	// the disk this test's own upgrade is about to swap.
	conf := fmt.Sprintf(
		"parity %s\ncontent %s\ncontent %s\ndata d1 %s/\ndata d2 %s/\n",
		filepath.Join(parityMount, "p116-victim.parity"),
		filepath.Join(parityMount, "p116-victim.content"),
		filepath.Join(victimWhere, "p116-victim.content"),
		upgradingWhere,
		victimWhere,
	)
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatalf("writing %s: %v", confPath, err)
	}
	engine := &parity.SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(filepath.Dir(confPath), "logs"),
		Runner:   parity.CommandRunner{},
		Guard:    parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}

	kept := filepath.Join(victimWhere, "docs", "kept-at-sync.bin")
	original := writeRandomLabFile(t, kept, 200_000)
	origHash := sha256HexLab(original)
	writeRandomLabFile(t, filepath.Join(upgradingWhere, "movies", "unrelated.bin"), 100_000)
	syncOnceDataUpgrade(t, ctx, engine)

	status, err := engine.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	label, ok := status.DataDiskLabel(victimWhere)
	if !ok {
		t.Fatalf("DataDiskLabel(%s) not found in %+v", victimWhere, status.DataMounts)
	}

	oldUUID, err := FilesystemUUID(ctx, r, upgradingDev)
	if err != nil {
		t.Fatalf("FilesystemUUID (upgrading disk): %v", err)
	}
	spec := DataDiskUpgradeSpec{
		Old:           MountUnit{Where: upgradingWhere, UUID: oldUUID, Filesystem: XFS},
		New:           DiskAddition{Device: newDev, Filesystem: XFS},
		NewFilesystem: XFS,
		Staging:       staging,
	}
	// This upgrade is left interrupted mid-copy and never resumed within
	// this test, so Diff/Release are never actually invoked — no-op
	// stubs, just enough to satisfy DataDiskUpgradeDeps' own
	// required-fields check.
	deps := DataDiskUpgradeDeps{
		Provider: provider,
		Runner:   r,
		Mounter:  mounter,
		ConfirmMounted: func(ctx context.Context, where, uuid string) error {
			return ConfirmMountedUUID(ctx, r, where, uuid)
		},
		Diff:    func(context.Context) (int, int, error) { return 0, 0, nil },
		Release: func(context.Context) error { return nil },
	}

	// Interrupt the unrelated upgrade partway through its own copy, then
	// leave it interrupted — mirroring an upgrade genuinely still in
	// flight at the moment the victim disk fails.
	stop := make(chan struct{})
	var logCalls int
	hooks := DataDiskUpgradeHooks{
		StopRequested: stop,
		Log: func(format string, args ...any) {
			logCalls++
			if logCalls == 1 {
				close(stop)
			}
		},
	}
	if _, err := RunDataDiskUpgrade(ctx, spec, deps, hooks, nil); err != nil {
		t.Fatalf("RunDataDiskUpgrade (left interrupted): %v", err)
	}

	// Fail the victim disk for real: unmount and detach its own loop
	// device, then mount a fresh, empty one at the exact same
	// mountpoint (doc 02 §4 "Replacing a failed disk" step 3).
	victimDevBefore := findmntSource(t, victimWhere)
	assertOwnLoopDataUpgrade(t, victimDevBefore, filepath.Join(lab, "img", "p116-data-victim-uninvolved.img"))
	if out, err := exec.Command("sync").CombinedOutput(); err != nil {
		t.Fatalf("sync: %v: %s", err, out)
	}
	if out, err := exec.Command("umount", victimWhere).CombinedOutput(); err != nil {
		t.Fatalf("umount %s: %v: %s", victimWhere, err, out)
	}
	if out, err := exec.Command("losetup", "-d", victimDevBefore).CombinedOutput(); err != nil {
		t.Fatalf("losetup -d %s: %v: %s", victimDevBefore, err, out)
	}
	createFormattedMountedDataDisk(t, lab, "p116-data-victim-replacement", victimWhere, "330M")

	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Fatalf("replacement disk is not empty before fix runs: stat %s: err=%v", kept, err)
	}

	ch, err := engine.Fix(ctx, parity.FixOpts{Disk: label})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	var final parity.Progress
	for p := range ch {
		final = p
	}
	if final.Err != nil {
		t.Fatalf("Fix failed: %v", final.Err)
	}

	restored, err := os.ReadFile(kept)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if sha256HexLab(restored) != origHash {
		t.Fatal("restored file does not match its pre-failure content — the in-progress, unrelated upgrade broke recovery")
	}

	checkCh, err := engine.Check(ctx, parity.CheckOpts{Disk: label})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	var checkFinal parity.Progress
	for p := range checkCh {
		checkFinal = p
	}
	if checkFinal.Err != nil {
		t.Fatalf("Check after fix reported a failure: %v", checkFinal.Err)
	}
}
