//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host — same pattern as snapraid_lab_test.go and guard_lab_test.go:
// built with `go test -tags lab -c` from the host (compiling touches no
// device) and run with `docker compose exec -T lab <binary>` inside the
// lab container. It exercises this issue's own lab acceptance criteria
// (doc 02 §4): adding a data disk to a running array triggers no rebuild
// of the disks already there, and replacing a failed disk — detach,
// replace, `snapraid fix -d`, verify — reconstructs its pre-failure
// contents byte for byte while the real change journal honestly names
// what was written after the last sync and so cannot be recovered.
//
// A dedicated fourth data disk is added and later failed here, rather
// than touching disk1-3 of `make lab-up`'s own standing array: those three
// are shared, accumulated state across every other lab test file in this
// package (snapraid_lab_test.go, guard_lab_test.go, journal_lab_test.go),
// all of which run in the same test binary and expect disk1-3 to survive
// intact between them. Detaching one of them here for real, as this
// issue's "replace a failed disk" scenario requires, would corrupt
// whichever of those tests runs afterward — so this file brings its own
// disk into the array, fails only that one, and leaves disk1-3 completely
// untouched throughout.

package parity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// labEngineWithMounts is labEngine (snapraid_lab_test.go) generalized to
// an arbitrary, caller-supplied set of data mounts, backed by its own
// parity and content files named after filesetName — distinct both from
// labEngine's own "snapraid.parity"/"snapraid.content" and from any other
// filesetName this file uses — so this file's own sync runs never rewrite
// the shared array's own content file into a shape (a fourth data
// directive) the three-disk labEngine every other lab test in this
// package (snapraid_lab_test.go, guard_lab_test.go, journal_lab_test.go)
// depends on could no longer read, and so this file's own two lifecycle
// tests — each growing the array by a different, unrelated fourth disk —
// never collide with each other's content file either. A distinct file
// name is all real SnapRAID needs to treat each as a wholly independent
// array, coexisting on the same physical disks without disturbing one
// another.
func labEngineWithMounts(t *testing.T, lab, workDirName, filesetName string, mounts []string) *SnapraidEngine {
	t.Helper()
	parity := filepath.Join(lab, "mnt/parity1")
	cache := filepath.Join(lab, "mnt/cache")

	var conf strings.Builder
	fmt.Fprintf(&conf, "parity %s\n", filepath.Join(parity, filesetName+".parity"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(parity, filesetName+".content"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(cache, filesetName+".content"))
	for i, m := range mounts {
		fmt.Fprintf(&conf, "data d%d %s\n", i+1, m+"/")
	}

	workDir := filepath.Join(lab, workDirName)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", workDir, err)
	}
	confPath := filepath.Join(workDir, "snapraid.conf")
	if err := os.WriteFile(confPath, []byte(conf.String()), 0o644); err != nil {
		t.Fatalf("writing %s: %v", confPath, err)
	}

	return &SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(workDir, "logs"),
		Runner:   CommandRunner{},
		// Same rationale as labEngine's own Guard override: this lab's
		// array is tiny by construction, so a handful of scripted file
		// changes routinely exceeds the guard's production thresholds for
		// reasons unrelated to what this test exercises.
		Guard: Guard{Config: GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}
}

// createAndMountLoopDisk truncates a fresh sparse image under lab's own
// img/ directory, attaches and formats it XFS, and mounts it at
// mountpoint — the same recipe create-array.sh uses for every other lab
// disk (doc 06 §3), confirming at each step that the device this function
// is about to touch is genuinely the loop device it just attached to the
// image it just created (lib.sh's own lab_assert_own_loop safety check,
// reimplemented here as assertOwnLoop already is elsewhere in this
// package's lab tests).
func createAndMountLoopDisk(t *testing.T, lab, name, mountpoint string) (dev string) {
	t.Helper()

	imgDir := filepath.Join(lab, "img")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", imgDir, err)
	}
	img := filepath.Join(imgDir, name+".img")
	if err := os.Remove(img); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing stale %s: %v", img, err)
	}
	if out, err := exec.Command("truncate", "-s", "320M", img).CombinedOutput(); err != nil {
		t.Fatalf("truncate %s: %v: %s", img, err, out)
	}

	out, err := exec.Command("losetup", "--find", "--show", img).Output()
	if err != nil {
		t.Fatalf("losetup --find --show %s: %v", img, err)
	}
	dev = strings.TrimSpace(string(out))
	assertOwnLoop(t, dev, img)

	if out, err := exec.Command("mkfs.xfs", "-q", dev).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.xfs %s: %v: %s", dev, err, out)
	}
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", mountpoint, err)
	}
	if out, err := exec.Command("mount", dev, mountpoint).CombinedOutput(); err != nil {
		t.Fatalf("mount %s %s: %v: %s", dev, mountpoint, err, out)
	}
	// Detach only this device, backed by the image this call just
	// created — never losetup -D (CLAUDE.md). A caller that has already
	// unmounted or detached dev by the time this runs (the replace test's
	// own mid-test failure injection) leaves both commands here as
	// harmless no-ops against an already-gone target. Loop device numbers
	// are host-global and freed as soon as they are detached, so this
	// re-checks ownership straight from sysfs rather than trusting dev
	// still names this test's own device — a concurrently running lab
	// may have since claimed the freed number for its own image.
	t.Cleanup(func() {
		if !loopStillBacksImage(t, dev, img) {
			t.Logf("skipping unmount/detach of %s: no longer backed by %s (likely reused by another lab)", dev, img)
			return
		}
		_, _ = exec.Command("umount", mountpoint).CombinedOutput()
		_, _ = exec.Command("losetup", "-d", dev).CombinedOutput()
	})
	return dev
}

// loopOwnerSysfsRoot stands in for /sys/block; overridden by
// TestLabLoopStillBacksImage_RefusesReusedDevice to inject a temp
// directory so the ownership check below can be exercised without a
// real loop device.
var loopOwnerSysfsRoot = "/sys/block"

// loopStillBacksImage reports whether dev is still the loop device
// backing img, read straight from
// /sys/block/<loopN>/loop/backing_file rather than trusted from when
// this test first attached it. Loop device numbers are host-global
// (doc 06 §3, Q45) and freed the moment another process detaches them,
// so by the time a t.Cleanup runs, dev may already belong to a
// concurrently running lab's own image — CLAUDE.md's "detach only the
// ones backed by your own lab's image files".
func loopStillBacksImage(t *testing.T, dev, img string) bool {
	t.Helper()
	loopName := filepath.Base(dev)
	if !strings.HasPrefix(loopName, "loop") {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(loopOwnerSysfsRoot, loopName, "loop", "backing_file"))
	if err != nil {
		// Not attached any more — nothing left for dev to own.
		return false
	}
	backing := strings.TrimSuffix(strings.TrimSpace(string(raw)), " (deleted)")

	wantImg, err := filepath.EvalSymlinks(img)
	if err != nil {
		wantImg = filepath.Clean(img)
	}
	gotImg, err := filepath.EvalSymlinks(backing)
	if err != nil {
		gotImg = filepath.Clean(backing)
	}
	return gotImg == wantImg
}

// TestLabLoopStillBacksImage_RefusesReusedDevice is this issue's own
// acceptance test (#383): a temp directory stands in for /sys/block, so
// this runs on any machine, not only inside the lab. A device number
// whose backing file no longer names this helper's own image — the
// shape left once a concurrently running lab reuses a freed loop
// number — is refused, never trusted from when it was first attached.
func TestLabLoopStillBacksImage_RefusesReusedDevice(t *testing.T) {
	root := t.TempDir()
	orig := loopOwnerSysfsRoot
	loopOwnerSysfsRoot = root
	t.Cleanup(func() { loopOwnerSysfsRoot = orig })

	ownImg := filepath.Join(t.TempDir(), "own.img")
	if err := os.WriteFile(ownImg, nil, 0o644); err != nil {
		t.Fatalf("writing %s: %v", ownImg, err)
	}
	otherImg := filepath.Join(t.TempDir(), "other-lab.img")
	if err := os.WriteFile(otherImg, nil, 0o644); err != nil {
		t.Fatalf("writing %s: %v", otherImg, err)
	}

	loopDir := filepath.Join(root, "loop7", "loop")
	if err := os.MkdirAll(loopDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", loopDir, err)
	}
	backingFile := filepath.Join(loopDir, "backing_file")

	if err := os.WriteFile(backingFile, []byte(ownImg+"\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", backingFile, err)
	}
	if !loopStillBacksImage(t, "/dev/loop7", ownImg) {
		t.Fatalf("loopStillBacksImage(own image) = false, want true")
	}

	if err := os.WriteFile(backingFile, []byte(otherImg+"\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", backingFile, err)
	}
	if loopStillBacksImage(t, "/dev/loop7", ownImg) {
		t.Fatalf("loopStillBacksImage(reused device) = true, want false — /dev/loop7 no longer backs this lab's image")
	}

	if err := os.RemoveAll(filepath.Join(root, "loop7", "loop")); err != nil {
		t.Fatalf("removing %s: %v", filepath.Join(root, "loop7", "loop"), err)
	}
	if loopStillBacksImage(t, "/dev/loop7", ownImg) {
		t.Fatalf("loopStillBacksImage(detached device) = true, want false — no loop/ subdirectory means not attached")
	}
}

// TestLabAddDisk_NewDiskJoinsWithoutTouchingExistingDisks is this issue's
// own "adding a disk triggers no rebuild" acceptance criterion (doc 02
// §4): a fourth data disk, added to the running array's own data list and
// synced, leaves the three disks already there completely unchanged —
// same file counts before and after, in the very diff that reports the
// new disk's own file as Added.
func TestLabAddDisk_NewDiskJoinsWithoutTouchingExistingDisks(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	_, mounts := labEngine(t, lab)

	// This test's own isolated array (distinct content/parity files, see
	// labEngineWithMounts) starts as the same three data disks the
	// standing array already has — reusing their directories to read and
	// write from is harmless; only this array's own, separate content
	// file ever changes shape. Establish a clean, fully-synced baseline
	// for it before this test's new disk ever exists.
	preAdd := labEngineWithMounts(t, lab, "p32-lifecycle-pre", "p32-add", mounts)
	for i, m := range mounts {
		writeFile(t, filepath.Join(m, fmt.Sprintf("lifecycle/baseline%d.bin", i)), 40_000)
	}
	syncOnce(t, ctx, preAdd)

	before, err := preAdd.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff before adding the disk: %v", err)
	}
	if before.Added != 0 || before.Removed != 0 || before.Updated != 0 {
		t.Fatalf("Diff before adding the disk reported pending changes: %+v, want none", before)
	}

	newMount := filepath.Join(lab, "mnt", "disk-p32-added")
	createAndMountLoopDisk(t, lab, "p32-add-disk", newMount)

	// Same filesetName ("p32-add") as preAdd, so this shares its own
	// content/parity files — only the conf's own data-directive list
	// grows to include the new disk, exactly doc 02 §4 step 6's
	// "regenerate configs" for an existing array.
	grown := append(append([]string{}, mounts...), newMount)
	engine := labEngineWithMounts(t, lab, "p32-lifecycle-grown", "p32-add", grown)

	writeFile(t, filepath.Join(newMount, "movies/new-disk-file.bin"), 60_000)

	diff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after adding the disk: %v", err)
	}
	if diff.Added != 1 {
		t.Fatalf("Diff after adding the disk: Added = %d, want 1 (only the new disk's own file)", diff.Added)
	}
	for i, m := range mounts {
		dd, ok := diff.PerDisk[filepath.Clean(m)]
		if !ok {
			t.Fatalf("Diff PerDisk missing existing disk %d (%s): %+v", i+1, m, diff.PerDisk)
		}
		if dd.FilesBefore != dd.FilesAfter {
			t.Fatalf("existing disk %d (%s): FilesBefore=%d FilesAfter=%d, want equal — no rebuild means existing disks are untouched", i+1, m, dd.FilesBefore, dd.FilesAfter)
		}
	}
	newDD, ok := diff.PerDisk[filepath.Clean(newMount)]
	if !ok || newDD.FilesAfter != 1 {
		t.Fatalf("Diff PerDisk for the new disk = %+v (ok=%v), want FilesAfter=1", newDD, ok)
	}

	ch, err := engine.Sync(ctx, SyncOpts{})
	if err != nil {
		t.Fatalf("Sync after adding the disk: %v", err)
	}
	final := drainReal(t, ch)
	if final.Err != nil {
		t.Fatalf("Sync after adding the disk failed: %v", final.Err)
	}

	// Nothing is left pending anywhere in the grown array — the new
	// disk's own file is now covered by parity, and the disks already
	// there needed no separate recompute to get there.
	afterSync, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after sync: %v", err)
	}
	if afterSync.Added != 0 || afterSync.Removed != 0 || afterSync.Updated != 0 {
		t.Fatalf("Diff after sync reported changes: %+v, want none", afterSync)
	}
}

// TestLabReplace_FixReconstructsAfterDiskLossWithHonestPendingFiles is
// this issue's own central safety-critical lab test (doc 02 §4
// "Replacing a failed disk"): a real disk is added to the array and
// synced, a real fanotify journal watches it, a file is written after
// that sync (what reconstruction can never recover), the disk is then
// genuinely failed — unmounted and its loop device detached, doc 06 §3's
// own "kill a disk mid-operation" recipe — replaced with a fresh loop
// device mounted at the exact same mountpoint, and `snapraid fix -d`
// reconstructs everything present at the last sync. Before the fix runs,
// the journal is confirmed to have already named the one file that fix
// cannot bring back — the honest constraint doc 02 §4 requires the API to
// surface.
func TestLabReplace_FixReconstructsAfterDiskLossWithHonestPendingFiles(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	_, mounts := labEngine(t, lab)

	failMount := filepath.Join(lab, "mnt", "disk-p32-replace")
	createAndMountLoopDisk(t, lab, "p32-replace-original", failMount)

	grown := append(append([]string{}, mounts...), failMount)
	engine := labEngineWithMounts(t, lab, "p32-replace-test", "p32-replace", grown)
	label := fmt.Sprintf("d%d", len(grown))

	kept := filepath.Join(failMount, "docs/kept-at-sync.bin")
	original := writeFile(t, kept, 200_000)
	origHash := sha256Hex(original)
	syncOnce(t, ctx, engine)

	status, err := engine.Status(ctx)
	if err != nil {
		t.Fatalf("Status after sync: %v", err)
	}
	if got, ok := status.DataDiskLabel(failMount); !ok || got != label {
		t.Fatalf("DataDiskLabel(%s) = (%q, %v), want (%q, true)", failMount, got, ok, label)
	}
	lastSyncAt := status.LastSyncAt

	journal := NewJournal(FanotifyWatcher{}, "")
	t.Cleanup(func() { _ = journal.Close() })
	if err := journal.AddDisk(ctx, label, failMount); err != nil {
		t.Fatalf("journal.AddDisk: %v", err)
	}

	// Written after the last successful sync: this is what a fix can
	// never restore, and what the journal must report before the fix
	// runs (doc 02 §4's own honest-constraint requirement).
	lostPath := filepath.Join(failMount, "docs/written-after-sync.bin")
	writeFile(t, lostPath, 50_000)

	waitFor(t, 5*time.Second, func() bool {
		s, err := journal.Summary(label)
		return err == nil && s.Count >= 1
	})

	pending, err := FixPendingFiles(journal, label, lastSyncAt)
	if err != nil {
		t.Fatalf("FixPendingFiles: %v", err)
	}
	if !pending.LastSyncAt.Equal(lastSyncAt) {
		t.Fatalf("FixPendingFiles.LastSyncAt = %v, want %v", pending.LastSyncAt, lastSyncAt)
	}
	foundLost := false
	for _, f := range pending.Files {
		if f.Name == "written-after-sync.bin" {
			foundLost = true
		}
	}
	if !foundLost {
		t.Fatalf("FixPendingFiles.Files = %+v, missing written-after-sync.bin — the one file a fix cannot recover must be named before the fix runs", pending.Files)
	}
	if pending.Incomplete {
		t.Fatal("FixPendingFiles.Incomplete = true, want false — no overflow occurred")
	}

	// Fail the disk for real: stop watching it first (its mountpoint is
	// about to disappear from under the mark), then unmount and detach
	// its own loop device — this lab's own image, never another lab's or
	// the host's (doc 06 §3, CLAUDE.md: never losetup -D).
	if err := journal.RemoveDisk(label); err != nil {
		t.Fatalf("journal.RemoveDisk: %v", err)
	}
	devOut, err := exec.Command("findmnt", "-n", "-o", "SOURCE", "--target", failMount).Output()
	if err != nil {
		t.Fatalf("findmnt --target %s: %v", failMount, err)
	}
	dev := strings.TrimSpace(string(devOut))
	img := filepath.Join(lab, "img", "p32-replace-original.img")
	assertOwnLoop(t, dev, img)

	if out, err := exec.Command("sync").CombinedOutput(); err != nil {
		t.Fatalf("sync: %v: %s", err, out)
	}
	if out, err := exec.Command("umount", failMount).CombinedOutput(); err != nil {
		t.Fatalf("umount %s: %v: %s", failMount, err, out)
	}
	if out, err := exec.Command("losetup", "-d", dev).CombinedOutput(); err != nil {
		t.Fatalf("losetup -d %s: %v: %s", dev, err, out)
	}

	// "Physically swap" the disk: a fresh loop device, freshly formatted,
	// mounted back at the exact same mountpoint the failed disk used
	// (doc 02 §4 "Replacing a failed disk" step 3).
	createAndMountLoopDisk(t, lab, "p32-replace-new", failMount)

	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Fatalf("replacement disk is not empty before fix runs: stat %s: err=%v", kept, err)
	}

	ch, err := engine.Fix(ctx, FixOpts{Disk: label})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	final := drainReal(t, ch)
	if final.Err != nil {
		t.Fatalf("Fix failed: %v", final.Err)
	}

	restored, err := os.ReadFile(kept)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if got := sha256Hex(restored); got != origHash {
		t.Fatalf("restored file sha256 = %s, want %s (the pre-failure hash)", got, origHash)
	}

	// The honest constraint, proven rather than merely stated: the file
	// written after the last sync is genuinely gone — fix cannot bring
	// back what parity never covered.
	if _, err := os.Stat(lostPath); !os.IsNotExist(err) {
		t.Fatalf("written-after-sync.bin survived the fix (err=%v) — it was never part of the synced state fix reconstructs from", err)
	}

	checkCh, err := engine.Check(ctx, CheckOpts{Disk: label})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	checkFinal := drainReal(t, checkCh)
	if checkFinal.Err != nil {
		t.Fatalf("Check after fix reported a failure: %v", checkFinal.Err)
	}
}
