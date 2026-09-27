//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container. It is
// this issue's own lab acceptance criterion (#54): interrupt a
// RelocateToCache run between its copy phase and its sync, fail a
// different, uninvolved data disk for real, and confirm `snapraid fix`
// reconstructs it fully — proving the two-phase order (Q14) genuinely
// keeps an in-flight, unsynced relocation from ever weakening the
// array's own recoverability.
//
// A dedicated fourth data disk carries the file that gets failed, rather
// than disk1-3 of `make lab-up`'s own standing array, for the same
// reason internal/parity's own lifecycle_lab_test.go isolates its
// failure injection onto a throwaway disk: disk1-3 are shared,
// accumulated state other lab test files in this package (mover_lab_test.go)
// still rely on surviving intact.

package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/parity"
)

func relocateLabDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	return filepath.Join("/lab", id)
}

// relocateLabEngineWithMounts is a copy of internal/parity's own
// labEngineWithMounts (lifecycle_lab_test.go) — that helper is
// unexported and lives in a different package, so this file cannot call
// it directly; the recipe (own parity/content files named after
// filesetName, generous guard thresholds for this lab's own tiny array)
// is reproduced exactly rather than approximated.
func relocateLabEngineWithMounts(t *testing.T, lab, workDirName, filesetName string, mounts []string) *parity.SnapraidEngine {
	t.Helper()
	parityDir := filepath.Join(lab, "mnt/parity1")
	cache := filepath.Join(lab, "mnt/cache")

	var conf strings.Builder
	fmt.Fprintf(&conf, "parity %s\n", filepath.Join(parityDir, filesetName+".parity"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(parityDir, filesetName+".content"))
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

	return &parity.SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(workDir, "logs"),
		Runner:   parity.CommandRunner{},
		Guard:    parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}
}

// relocateCreateAndMountLoopDisk is internal/parity's own
// createAndMountLoopDisk (lifecycle_lab_test.go), reproduced here for
// the same reason relocateLabEngineWithMounts is: an unexported helper
// in a different package this file cannot import.
func relocateCreateAndMountLoopDisk(t *testing.T, lab, name, mountpoint string) {
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
	dev := strings.TrimSpace(string(out))
	relocateAssertOwnLoop(t, dev, img)

	if out, err := exec.Command("mkfs.xfs", "-q", dev).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.xfs %s: %v: %s", dev, err, out)
	}
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", mountpoint, err)
	}
	if out, err := exec.Command("mount", dev, mountpoint).CombinedOutput(); err != nil {
		t.Fatalf("mount %s %s: %v: %s", dev, mountpoint, err, out)
	}
	// Loop device numbers are host-global and freed as soon as they are
	// detached, so this re-checks ownership straight from sysfs rather
	// than trusting dev still names this test's own device — a
	// concurrently running lab may have since claimed the freed number
	// for its own image (CLAUDE.md: detach only devices backed by your
	// own lab's image files). cacheLoopStillBacksImage is shared with
	// rebalance_lab_test.go's own rebalanceCreateAndMountLoopDisk.
	t.Cleanup(func() {
		if !cacheLoopStillBacksImage(t, dev, img) {
			t.Logf("skipping unmount/detach of %s: no longer backed by %s (likely reused by another lab)", dev, img)
			return
		}
		_, _ = exec.Command("umount", mountpoint).CombinedOutput()
		_, _ = exec.Command("losetup", "-d", dev).CombinedOutput()
	})
}

// relocateAssertOwnLoop refuses any device that is not the loop device
// backing img — never losetup -D (CLAUDE.md) — the same check
// internal/parity's own assertOwnLoop performs.
func relocateAssertOwnLoop(t *testing.T, dev, img string) {
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

// cacheLoopOwnerSysfsRoot stands in for /sys/block; overridden by
// TestLabLoopStillBacksImage_RefusesReusedDevice to inject a temp
// directory so the ownership check below can be exercised without a
// real loop device. Shared by this package's two lab-tagged files that
// pair a loop-device detach with an unmount — relocate_lab_test.go and
// rebalance_lab_test.go — so neither carries its own copy.
var cacheLoopOwnerSysfsRoot = "/sys/block"

// cacheLoopStillBacksImage reports whether dev is still the loop
// device backing img, read straight from
// /sys/block/<loopN>/loop/backing_file rather than trusted from when
// this test first attached it. Loop device numbers are host-global
// (doc 06 §3, Q45) and freed the moment another process detaches them,
// so by the time a t.Cleanup runs, dev may already belong to a
// concurrently running lab's own image — CLAUDE.md's "detach only the
// ones backed by your own lab's image files".
func cacheLoopStillBacksImage(t *testing.T, dev, img string) bool {
	t.Helper()
	loopName := filepath.Base(dev)
	if !strings.HasPrefix(loopName, "loop") {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(cacheLoopOwnerSysfsRoot, loopName, "loop", "backing_file"))
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
	orig := cacheLoopOwnerSysfsRoot
	cacheLoopOwnerSysfsRoot = root
	t.Cleanup(func() { cacheLoopOwnerSysfsRoot = orig })

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
	if !cacheLoopStillBacksImage(t, "/dev/loop7", ownImg) {
		t.Fatalf("cacheLoopStillBacksImage(own image) = false, want true")
	}

	if err := os.WriteFile(backingFile, []byte(otherImg+"\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", backingFile, err)
	}
	if cacheLoopStillBacksImage(t, "/dev/loop7", ownImg) {
		t.Fatalf("cacheLoopStillBacksImage(reused device) = true, want false — /dev/loop7 no longer backs this lab's image")
	}

	if err := os.RemoveAll(filepath.Join(root, "loop7", "loop")); err != nil {
		t.Fatalf("removing %s: %v", filepath.Join(root, "loop7", "loop"), err)
	}
	if cacheLoopStillBacksImage(t, "/dev/loop7", ownImg) {
		t.Fatalf("cacheLoopStillBacksImage(detached device) = true, want false — no loop/ subdirectory means not attached")
	}
}

func relocateWriteFile(t *testing.T, path string, sizeBytes int) []byte {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	data := make([]byte, sizeBytes)
	f, err := os.Open("/dev/urandom")
	if err != nil {
		t.Fatalf("open /dev/urandom: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.ReadFull(f, data); err != nil {
		t.Fatalf("reading random data: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return data
}

func relocateSha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func relocateDrainReal(t *testing.T, ch <-chan parity.Progress) parity.Progress {
	t.Helper()
	var last parity.Progress
	for p := range ch {
		last = p
	}
	return last
}

func relocateSyncOnce(t *testing.T, ctx context.Context, engine *parity.SnapraidEngine) {
	t.Helper()
	ch, err := engine.Sync(ctx, parity.SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	final := relocateDrainReal(t, ch)
	if final.Err != nil {
		t.Fatalf("Sync failed: %v", final.Err)
	}
}

// TestLabRelocateToCache_InterruptedBeforeSync_OtherDiskReconstructsFully
// is this issue's own lab acceptance criterion: RelocateToCache is
// stopped right after its copy phase finishes — before it ever calls
// sync — leaving the array's own data disks completely untouched (cache
// is outside parity; nothing about writing to it changes any data disk's
// state). A different, uninvolved data disk is then genuinely failed —
// unmounted and its loop device detached — and replaced. `snapraid fix`
// reconstructs it byte-for-byte from the last real sync, proving the
// interrupted relocation never put that recovery at risk.
func TestLabRelocateToCache_InterruptedBeforeSync_OtherDiskReconstructsFully(t *testing.T) {
	lab := relocateLabDir(t)
	ctx := context.Background()

	shareDisks := []string{
		filepath.Join(lab, "mnt/disk1"),
		filepath.Join(lab, "mnt/disk2"),
		filepath.Join(lab, "mnt/disk3"),
	}

	failMount := filepath.Join(lab, "mnt", "disk-p54-other")
	relocateCreateAndMountLoopDisk(t, lab, "p54-other-original", failMount)

	allMounts := append(append([]string{}, shareDisks...), failMount)
	engine := relocateLabEngineWithMounts(t, lab, "p54-relocate-test", "p54-relocate", allMounts)
	failLabel := fmt.Sprintf("d%d", len(allMounts))

	// The share's own array-side file, on disk1 — what RelocateToCache
	// will copy to cache in this test.
	shareName := "relocatedocs"
	for _, d := range shareDisks {
		if err := os.MkdirAll(filepath.Join(d, shareName), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	relocatedSrc := filepath.Join(shareDisks[0], shareName, "moved.bin")
	relocateWriteFile(t, relocatedSrc, 300_000)

	// A second file on disk1, outside the relocated share entirely, so
	// relocating moved.bin away doesn't drop disk1 to zero files: the
	// guard's zero-files rule (doc 02 §2) is deliberately separate from
	// Q15's own manifest matching — it is the "did a disk go missing"
	// check, and fires regardless of whether the removal is otherwise
	// accounted for. This test is about the interrupt/resume guarantee,
	// not that rule, so disk1 keeps content of its own throughout.
	stableOnDisk1 := filepath.Join(shareDisks[0], "other", "stays.bin")
	relocateWriteFile(t, stableOnDisk1, 50_000)

	// The unrelated file, on the disk this test is about to fail — kept
	// completely outside the relocation, so its own reconstruction
	// proves the interrupted relocation elsewhere never touched it.
	kept := filepath.Join(failMount, "docs/kept.bin")
	original := relocateWriteFile(t, kept, 250_000)
	origHash := relocateSha256Hex(original)

	relocateSyncOnce(t, ctx, engine)

	before, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff before relocation: %v", err)
	}
	if before.Added != 0 || before.Removed != 0 || before.Updated != 0 {
		t.Fatalf("Diff before relocation reported pending changes: %+v, want none", before)
	}

	cachePath := filepath.Join(lab, "mnt/cache", shareName)
	if err := os.MkdirAll(cachePath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", cachePath, err)
	}
	share := Share{
		Name:      shareName,
		CachePath: cachePath,
		Branches: []string{
			filepath.Join(shareDisks[0], shareName),
			filepath.Join(shareDisks[1], shareName),
			filepath.Join(shareDisks[2], shareName),
		},
	}

	syncCalled := false
	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		syncCalled = true
		return fmt.Errorf("sync must never be called before this test's own interrupt point")
	}

	relocCtx, cancel := context.WithCancel(ctx)
	var lastCheckpoint []byte
	hooks := RunHooks{
		SaveCheckpoint: func(data []byte) error {
			lastCheckpoint = append([]byte(nil), data...)
			var cp RelocateCheckpoint
			if err := json.Unmarshal(data, &cp); err != nil {
				t.Fatalf("unmarshal checkpoint: %v", err)
			}
			if cp.Phase == RelocatePhaseSyncing {
				cancel()
			}
			return nil
		},
	}

	report, err := RelocateToCache(relocCtx, share, Config{}, deps, hooks, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	if !report.Interrupted {
		t.Fatalf("expected the run to report Interrupted, got %+v", report)
	}
	if syncCalled {
		t.Fatal("sync must never have been called before the interrupt")
	}
	if len(lastCheckpoint) == 0 {
		t.Fatal("expected a saved checkpoint at the copy/sync boundary")
	}

	// The array is completely untouched at this point: the relocated
	// file is still on disk1, and the copy on cache is a pure,
	// unsynced-and-irrelevant-to-parity duplicate.
	if _, err := os.Stat(relocatedSrc); err != nil {
		t.Fatalf("array original must survive an interrupt before sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cachePath, "moved.bin")); err != nil {
		t.Fatalf("verified cache copy must exist: %v", err)
	}

	stillClean, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after the interrupted relocation: %v", err)
	}
	if stillClean.Added != 0 || stillClean.Removed != 0 || stillClean.Updated != 0 {
		t.Fatalf("Diff after the interrupted relocation reported changes: %+v, want none — cache is outside parity", stillClean)
	}

	// Fail a genuinely different data disk for real (doc 06 §3's own
	// "kill a disk mid-operation" recipe) — never the disks the
	// relocation itself touched.
	devOut, err := exec.Command("findmnt", "-n", "-o", "SOURCE", "--target", failMount).Output()
	if err != nil {
		t.Fatalf("findmnt --target %s: %v", failMount, err)
	}
	dev := strings.TrimSpace(string(devOut))
	img := filepath.Join(lab, "img", "p54-other-original.img")
	relocateAssertOwnLoop(t, dev, img)

	if out, err := exec.Command("sync").CombinedOutput(); err != nil {
		t.Fatalf("sync: %v: %s", err, out)
	}
	if out, err := exec.Command("umount", failMount).CombinedOutput(); err != nil {
		t.Fatalf("umount %s: %v: %s", failMount, err, out)
	}
	if out, err := exec.Command("losetup", "-d", dev).CombinedOutput(); err != nil {
		t.Fatalf("losetup -d %s: %v: %s", dev, err, out)
	}

	relocateCreateAndMountLoopDisk(t, lab, "p54-other-new", failMount)
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Fatalf("replacement disk is not empty before fix runs: stat %s: err=%v", kept, err)
	}

	ch, err := engine.Fix(ctx, parity.FixOpts{Disk: failLabel})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	final := relocateDrainReal(t, ch)
	if final.Err != nil {
		t.Fatalf("Fix failed: %v", final.Err)
	}

	restored, err := os.ReadFile(kept)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if got := relocateSha256Hex(restored); got != origHash {
		t.Fatalf("restored file sha256 = %s, want %s — the interrupted relocation elsewhere must never have compromised this disk's own recovery", got, origHash)
	}

	checkCh, err := engine.Check(ctx, parity.CheckOpts{Disk: failLabel})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	checkFinal := relocateDrainReal(t, checkCh)
	if checkFinal.Err != nil {
		t.Fatalf("Check after fix reported a failure: %v", checkFinal.Err)
	}

	// Finally, confirm the interrupted relocation can still be resumed
	// to completion once a real Sync hook is supplied — the checkpoint
	// this test cancelled at is not just inert, it is genuinely usable.
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		ch, err := engine.Sync(ctx, parity.SyncOpts{Manifest: manifest})
		if err != nil {
			return err
		}
		last := relocateDrainReal(t, ch)
		return last.Err
	}
	resumed, err := RelocateToCache(ctx, share, Config{}, deps, RunHooks{}, lastCheckpoint)
	if err != nil {
		t.Fatalf("resumed RelocateToCache: %v", err)
	}
	if len(resumed.Moved()) != 1 {
		t.Fatalf("expected the resumed relocation to complete, got %+v", resumed.Entries)
	}
	if _, err := os.Stat(relocatedSrc); !os.IsNotExist(err) {
		t.Fatalf("array original should be gone after the resumed relocation: err=%v", err)
	}
	if _, err := os.Stat(stableOnDisk1); err != nil {
		t.Fatalf("disk1's own unrelated file must survive the relocation and its trailing sync: %v", err)
	}
}
