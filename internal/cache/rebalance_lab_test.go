//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern relocate_lab_test.go and mover_lab_test.go use in this
// package. It carries this issue's own lab acceptance criteria (doc 09
// §6): a deliberately skewed pool gets evened out; an interrupted
// rebalance never deletes a source before the sync that covers its
// copy, proven by genuinely failing a different, uninvolved data disk
// and reconstructing it; and — the issue's own central sizing claim —
// batching a rebalance against the threshold guard's real,
// unmodified defaults lets an evacuation-sized removal complete
// unblocked while an unrelated mass deletion of comparable size, in the
// same window, still trips that same unmodified guard.
//
// TestLabRebalance_BatchedAgainstRealGuard_UnrelatedMassDeletionStillTrips
// brings up its own dedicated loop disks rather than touching disk1-3 of
// `make lab-up`'s own standing array, for the same reason
// relocate_lab_test.go and lifecycle_lab_test.go (internal/parity) do:
// this test's own array-wide tracked-file-count math has to be exact,
// and disk1-3 accumulate state from every other lab test file in this
// package.

package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/parity"
)

func rebalanceLabDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	return filepath.Join("/lab", id)
}

// rebalanceLabEngineWithMounts is relocateLabEngineWithMounts /
// internal/parity's own labEngineWithMounts, generalized to a
// caller-supplied Guard so this file's own guard-precision test can use
// a real, completely unmodified GuardConfig{} while its other tests use
// the same generous thresholds every other lab test file in this package
// relies on (their own tiny arrays routinely exceed production
// thresholds for reasons unrelated to what those tests exercise).
func rebalanceLabEngineWithMounts(t *testing.T, lab, workDirName, filesetName string, mounts []string, guard parity.Guard) *parity.SnapraidEngine {
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
	// The real, production exclude set (parity.DefaultExcludes) — never a
	// pared-down list — so this file's own untracked "junk" files
	// (below) are genuinely excluded from SnapRAID's scans the same way
	// they would be on a real array, rather than by some test-only
	// approximation of that list.
	for _, e := range parity.DefaultExcludes {
		fmt.Fprintf(&conf, "exclude %s\n", e)
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
		Guard:    guard,
	}
}

// rebalanceGenerousGuard is the same "this lab's own array is tiny by
// construction" override every other lab test file in this package
// applies, for the two tests here that are not themselves about the
// guard's own precise thresholds.
func rebalanceGenerousGuard() parity.Guard {
	return parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}}
}

// rebalanceCreateAndMountLoopDisk is relocateCreateAndMountLoopDisk /
// internal/parity's own createAndMountLoopDisk, reproduced here for the
// same reason those are: an unexported helper in a different package (or
// file already carrying its own name for this exact helper) this file
// cannot reuse directly.
func rebalanceCreateAndMountLoopDisk(t *testing.T, lab, name, mountpoint string) (dev string) {
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
	rebalanceAssertOwnLoop(t, dev, img)

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
	// own lab's image files). cacheLoopStillBacksImage is
	// relocate_lab_test.go's own helper, shared by this package's two
	// lab-tagged files that pair a loop-device detach with an unmount.
	t.Cleanup(func() {
		if !cacheLoopStillBacksImage(t, dev, img) {
			t.Logf("skipping unmount/detach of %s: no longer backed by %s (likely reused by another lab)", dev, img)
			return
		}
		_, _ = exec.Command("umount", mountpoint).CombinedOutput()
		_, _ = exec.Command("losetup", "-d", dev).CombinedOutput()
	})
	return dev
}

// rebalanceAssertOwnLoop refuses any device that is not the loop device
// backing img — never losetup -D (CLAUDE.md).
func rebalanceAssertOwnLoop(t *testing.T, dev, img string) {
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

func rebalanceLabDrain(t *testing.T, ch <-chan parity.Progress) parity.Progress {
	t.Helper()
	var last parity.Progress
	for p := range ch {
		last = p
	}
	return last
}

func rebalanceLabSyncOnce(t *testing.T, ctx context.Context, engine *parity.SnapraidEngine) {
	t.Helper()
	ch, err := engine.Sync(ctx, parity.SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if final := rebalanceLabDrain(t, ch); final.Err != nil {
		t.Fatalf("Sync failed: %v", final.Err)
	}
}

func rebalanceLabSyncFunc(engine *parity.SnapraidEngine) SyncFunc {
	return func(ctx context.Context, manifest []parity.ManifestEntry) error {
		ch, err := engine.Sync(ctx, parity.SyncOpts{Manifest: manifest})
		if err != nil {
			return err
		}
		var last parity.Progress
		for p := range ch {
			last = p
		}
		return last.Err
	}
}

// rebalanceLabTrackedFileCount adapts engine into Deps.TrackedFileCount
// exactly the way a real caller must (Deps.TrackedFileCount's own doc
// comment): a fresh Diff, summed across every disk precisely the way
// guard.go's own Evaluate sums "totalBefore" — never an independent
// filesystem walk.
func rebalanceLabTrackedFileCount(engine *parity.SnapraidEngine) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		diff, err := engine.Diff(ctx)
		if err != nil {
			return 0, err
		}
		total := 0
		for _, dd := range diff.PerDisk {
			total += dd.FilesBefore
		}
		return total, nil
	}
}

func rebalanceLabWriteFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// TestLabRebalance_EvensOutSkewedPool is doc 09 §6's own "rebalance
// evens out a deliberately skewed pool": a share with one disk heavily
// used and another nearly empty gets a real PlanRebalance, and running
// that plan for real (RunRebalance, against a real SnapraidEngine)
// brings the two disks within the default skew tolerance.
func TestLabRebalance_EvensOutSkewedPool(t *testing.T) {
	lab := rebalanceLabDir(t)
	ctx := context.Background()

	// Dedicated, freshly created disks — never disk1-3 of `make lab-up`'s
	// own standing array: this test's own skew and per-disk usage
	// assertions need a filesystem with no other lab test's own
	// leftover state on it, and — since a rebalance's own delete phase
	// really does remove files from these mounts — no risk of a stale
	// content-file/data-disk mismatch across repeated runs of this same
	// binary in one lab session either (doc 06 §3's own "run-once" rule
	// for anything that mutates on-disk state).
	mounts := []string{
		filepath.Join(lab, "mnt", "disk-p55-a1"),
		filepath.Join(lab, "mnt", "disk-p55-a2"),
		filepath.Join(lab, "mnt", "disk-p55-a3"),
	}
	for i, m := range mounts {
		rebalanceCreateAndMountLoopDisk(t, lab, fmt.Sprintf("p55-a%d", i+1), m)
	}
	engine := rebalanceLabEngineWithMounts(t, lab, "p55-plan-test", "p55-plan", mounts, rebalanceGenerousGuard())

	shareName := "skewshare"
	branches := make([]string, len(mounts))
	for i, m := range mounts {
		branches[i] = filepath.Join(m, shareName)
		if err := os.MkdirAll(branches[i], 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	// Several medium files, deliberately on the first branch only — well
	// past the default 5% skew tolerance on this lab's own small (320M)
	// disks. Several files, not one giant one: PlanRebalance can only
	// redistribute what it has candidates for, and a single file bigger
	// than half of what balancing the pool needs cannot converge no
	// matter how it's placed — it only relocates the imbalance, it
	// cannot split it. fallocate, not truncate: truncate's own sparse
	// file consumes no real blocks until written, so statfs would see
	// no usage at all — this needs real, allocated space to create a
	// real skew.
	const fillerCount, fillerSize = 12, "18M"
	for i := 0; i < fillerCount; i++ {
		filler := filepath.Join(branches[0], fmt.Sprintf("filler%02d.bin", i))
		if out, err := exec.Command("fallocate", "-l", fillerSize, filler).CombinedOutput(); err != nil {
			t.Fatalf("fallocate %s: %v: %s", filler, err, out)
		}
	}
	// A stable file on the same branch — draining it down to the pool's
	// own average must not drop this disk to zero tracked files: the
	// guard's zero-files rule is unconditional and fires regardless of
	// RemovedFilesMax/RemovedUpdatedPercent, and this test is about skew
	// reduction, not that rule.
	rebalanceLabWriteFile(t, filepath.Join(branches[0], "stays.bin"), "stays")

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Sync = rebalanceLabSyncFunc(engine)
	deps.TrackedFileCount = rebalanceLabTrackedFileCount(engine)
	// This test is about target selection, not batch sizing against the
	// guard — match the engine's own generous Guard.Config above so a
	// tiny tracked-file count (a handful of files) can never make
	// rebalanceBatchSize reject even this plan's single move.
	deps.RebalancePercentLimit = func() float64 { return 99 }

	share := Share{Name: shareName, Branches: branches}

	plan, err := PlanRebalance(ctx, []Share{share}, RebalanceConfig{}, deps)
	if err != nil {
		t.Fatalf("PlanRebalance: %v", err)
	}
	if len(plan.Moves) == 0 {
		t.Fatal("expected at least one move for a deliberately skewed pool")
	}

	report, err := RunRebalance(ctx, plan, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	if len(report.Moved()) != len(plan.Moves) {
		t.Fatalf("expected every planned move to complete, got %+v", report.Entries)
	}

	var usages []DiskUsage
	for _, b := range branches {
		u, err := UsageBytes(b)
		if err != nil {
			t.Fatalf("UsageBytes(%s): %v", b, err)
		}
		usages = append(usages, u)
	}
	most, least := usages[0].UsedPercent(), usages[0].UsedPercent()
	for _, u := range usages[1:] {
		if u.UsedPercent() > most {
			most = u.UsedPercent()
		}
		if u.UsedPercent() < least {
			least = u.UsedPercent()
		}
	}
	if skew := most - least; skew > DefaultSkewTolerancePercent {
		t.Fatalf("skew after rebalance = %.1f%%, want <= %.1f%%", skew, DefaultSkewTolerancePercent)
	}

	diff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after rebalance: %v", err)
	}
	if diff.Added != 0 || diff.Removed != 0 || diff.Updated != 0 {
		t.Fatalf("Diff after rebalance reported pending changes: %+v, want none", diff)
	}
}

// TestLabRebalance_InterruptedBeforeSync_OtherDiskReconstructsFully is
// doc 09 §6's own "rebalance and evacuation never delete a source before
// the sync that covers its copy": RunRebalance is stopped right after
// its copy phase finishes — before it ever calls sync — leaving the
// array's own data disks completely untouched (the copy's own target is
// also outside parity's own last-synced state until that sync runs). A
// different, uninvolved data disk is then genuinely failed — unmounted
// and its loop device detached — and replaced; `snapraid fix`
// reconstructs it byte-for-byte from the last real sync, proving the
// interrupted rebalance never put that recovery at risk. The run is then
// resumed to completion from the exact checkpoint it left behind.
func TestLabRebalance_InterruptedBeforeSync_OtherDiskReconstructsFully(t *testing.T) {
	lab := rebalanceLabDir(t)
	ctx := context.Background()

	sourceMount := filepath.Join(lab, "mnt", "disk-p55-b-source")
	targetMount := filepath.Join(lab, "mnt", "disk-p55-b-target")
	failMount := filepath.Join(lab, "mnt", "disk-p55-b-other")
	rebalanceCreateAndMountLoopDisk(t, lab, "p55-b-source", sourceMount)
	rebalanceCreateAndMountLoopDisk(t, lab, "p55-b-target", targetMount)
	rebalanceCreateAndMountLoopDisk(t, lab, "p55-b-other-original", failMount)

	allMounts := []string{sourceMount, targetMount, failMount}
	engine := rebalanceLabEngineWithMounts(t, lab, "p55-b-test", "p55-b", allMounts, rebalanceGenerousGuard())
	failLabel := "d3"

	shareName := "rebalanceit"
	sourceBranch := filepath.Join(sourceMount, shareName)
	targetBranch := filepath.Join(targetMount, shareName)
	for _, d := range []string{sourceBranch, targetBranch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	rel := "moved.bin"
	content := strings.Repeat("x", 300_000)
	rebalanceLabWriteFile(t, filepath.Join(sourceBranch, rel), content)

	// A second file on the source disk, outside the plan entirely, so
	// moving moved.bin away doesn't drop the source disk to zero files —
	// the guard's zero-files rule is a separate check from this test's
	// own interrupt/resume guarantee.
	stableOnSource := filepath.Join(sourceBranch, "stays.bin")
	rebalanceLabWriteFile(t, stableOnSource, "stays")

	kept := filepath.Join(failMount, "docs/kept.bin")
	original := strings.Repeat("k", 250_000)
	rebalanceLabWriteFile(t, kept, original)

	rebalanceLabSyncOnce(t, ctx, engine)

	before, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff before rebalance: %v", err)
	}
	if before.Added != 0 || before.Removed != 0 || before.Updated != 0 {
		t.Fatalf("Diff before rebalance reported pending changes: %+v, want none", before)
	}

	plan := RebalancePlan{Moves: []RebalanceMove{{
		Share:        shareName,
		RelPath:      rel,
		SourceBranch: sourceBranch,
		TargetBranch: targetBranch,
		Size:         int64(len(content)),
	}}}

	syncCalled := false
	deps := Deps{Open: NewFakeOpenChecker()}
	deps.TrackedFileCount = rebalanceLabTrackedFileCount(engine)
	// This test is about the interrupt/resume guarantee, not batch
	// sizing against the guard — match the engine's own generous
	// Guard.Config above so this tiny array's own handful of tracked
	// files can never make rebalanceBatchSize reject this plan's single
	// move.
	deps.RebalancePercentLimit = func() float64 { return 99 }
	deps.Sync = func(context.Context, []parity.ManifestEntry) error {
		syncCalled = true
		return fmt.Errorf("sync must never be called before this test's own interrupt point")
	}

	rebalCtx, cancel := context.WithCancel(ctx)
	var lastCheckpoint []byte
	hooks := RunHooks{
		SaveCheckpoint: func(data []byte) error {
			lastCheckpoint = append([]byte(nil), data...)
			var cp RebalanceCheckpoint
			if uerr := json.Unmarshal(data, &cp); uerr != nil {
				t.Fatalf("unmarshal checkpoint: %v", uerr)
			}
			if cp.Phase == RebalancePhaseSyncing {
				cancel()
			}
			return nil
		},
	}

	report, err := RunRebalance(rebalCtx, plan, Config{}, deps, hooks, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
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

	if _, err := os.Stat(filepath.Join(sourceBranch, rel)); err != nil {
		t.Fatalf("source must survive an interrupt before sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetBranch, rel)); err != nil {
		t.Fatalf("verified target copy must exist: %v", err)
	}

	stillClean, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after the interrupted rebalance: %v", err)
	}
	if stillClean.Added != 0 || stillClean.Removed != 0 || stillClean.Updated != 0 {
		t.Fatalf("Diff after the interrupted rebalance reported changes: %+v, want none — nothing was ever synced", stillClean)
	}

	devOut, err := exec.Command("findmnt", "-n", "-o", "SOURCE", "--target", failMount).Output()
	if err != nil {
		t.Fatalf("findmnt --target %s: %v", failMount, err)
	}
	dev := strings.TrimSpace(string(devOut))
	img := filepath.Join(lab, "img", "p55-b-other-original.img")
	rebalanceAssertOwnLoop(t, dev, img)

	if out, err := exec.Command("sync").CombinedOutput(); err != nil {
		t.Fatalf("sync: %v: %s", err, out)
	}
	if out, err := exec.Command("umount", failMount).CombinedOutput(); err != nil {
		t.Fatalf("umount %s: %v: %s", failMount, err, out)
	}
	if out, err := exec.Command("losetup", "-d", dev).CombinedOutput(); err != nil {
		t.Fatalf("losetup -d %s: %v: %s", dev, err, out)
	}

	rebalanceCreateAndMountLoopDisk(t, lab, "p55-b-other-new", failMount)
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Fatalf("replacement disk is not empty before fix runs: stat %s: err=%v", kept, err)
	}

	ch, err := engine.Fix(ctx, parity.FixOpts{Disk: failLabel})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if final := rebalanceLabDrain(t, ch); final.Err != nil {
		t.Fatalf("Fix failed: %v", final.Err)
	}

	restored, err := os.ReadFile(kept)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if string(restored) != original {
		t.Fatal("restored file content mismatch — the interrupted rebalance elsewhere must never have compromised this disk's own recovery")
	}

	checkCh, err := engine.Check(ctx, parity.CheckOpts{Disk: failLabel})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if final := rebalanceLabDrain(t, checkCh); final.Err != nil {
		t.Fatalf("Check after fix reported a failure: %v", final.Err)
	}

	deps.Sync = rebalanceLabSyncFunc(engine)
	resumed, err := RunRebalance(ctx, plan, Config{}, deps, RunHooks{}, lastCheckpoint)
	if err != nil {
		t.Fatalf("resumed RunRebalance: %v", err)
	}
	if len(resumed.Moved()) != 1 {
		t.Fatalf("expected the resumed rebalance to complete, got %+v", resumed.Entries)
	}
	if _, err := os.Stat(filepath.Join(sourceBranch, rel)); !os.IsNotExist(err) {
		t.Fatalf("source should be gone after the resumed rebalance: err=%v", err)
	}
	if _, err := os.Stat(stableOnSource); err != nil {
		t.Fatalf("source disk's own unrelated file must survive the rebalance and its trailing sync: %v", err)
	}
}

// TestLabRebalance_BatchedAgainstRealGuard_UnrelatedMassDeletionStillTrips
// is this issue's own central sizing claim (doc 09 §6's "evacuation does
// not trip the threshold guard, and an unrelated mass deletion during
// the same window still does", adapted from evacuation to rebalance):
// against a real, completely unmodified parity.GuardConfig{} (defaults
// RemovedFilesMax=500, RemovedUpdatedPercent=10.0), a rebalance large
// enough that a single unbatched sync would trip the percent rule
// completes unblocked once RunRebalance batches it against
// Deps.TrackedFileCount's own real, SnapRAID-tracked reading — even with
// untracked "junk" files physically present on the disks — while an
// unrelated mass deletion of comparable size, in the same array, in the
// same test, still trips that same unmodified guard.
func TestLabRebalance_BatchedAgainstRealGuard_UnrelatedMassDeletionStillTrips(t *testing.T) {
	lab := rebalanceLabDir(t)
	ctx := context.Background()

	d1 := filepath.Join(lab, "mnt", "disk-p55-c1")
	d2 := filepath.Join(lab, "mnt", "disk-p55-c2")
	d3 := filepath.Join(lab, "mnt", "disk-p55-c3")
	rebalanceCreateAndMountLoopDisk(t, lab, "p55-c1", d1)
	rebalanceCreateAndMountLoopDisk(t, lab, "p55-c2", d2)
	rebalanceCreateAndMountLoopDisk(t, lab, "p55-c3", d3)

	mounts := []string{d1, d2, d3}
	// The real, unmodified guard — never shrunk or inflated to fit this
	// test (CLAUDE.md: "guard tests may never be weakened").
	engine := rebalanceLabEngineWithMounts(t, lab, "p55-c-test", "p55-c", mounts, parity.Guard{})

	shareName := "media"
	branches := make([]string, len(mounts))
	for i, m := range mounts {
		branches[i] = filepath.Join(m, shareName)
	}

	const perDisk = 100 // 300 tracked files total across the three disks
	// Globally unique names across all three disks — branches[0]'s own
	// files are about to be moved onto branches[1], which must not
	// already hold a same-named file of its own or the copy phase's own
	// isSamePendingCopy check would see two unrelated files with the
	// same size and treat it as a conflict.
	for d, b := range branches {
		for i := 0; i < perDisk; i++ {
			n := d*perDisk + i
			rebalanceLabWriteFile(t, filepath.Join(b, fmt.Sprintf("file%03d.bin", n)), fmt.Sprintf("content-%03d", n))
		}
	}

	// Untracked "junk", matching parity.DefaultExcludes, on both disks
	// this test's own rebalance touches — proving batch sizing uses
	// SnapRAID's own tracked count, not a filesystem walk that would
	// count these and oversize a batch relative to the real guard.
	for _, b := range []string{branches[0], branches[1]} {
		for i := 0; i < 15; i++ {
			rebalanceLabWriteFile(t, filepath.Join(b, fmt.Sprintf("junk%02d.unrecoverable", i)), "junk")
		}
	}
	rebalanceLabWriteFile(t, filepath.Join(branches[0], ".DS_Store"), "junk")
	rebalanceLabWriteFile(t, filepath.Join(branches[1], ".DS_Store"), "junk")

	// The very first sync of a wholly empty array can never trip either
	// rule (removedCount=0, and totalBefore=0 keeps percent at 0 —
	// guard.go's Evaluate has no "before" state to compare against yet),
	// so this seeding sync needs no batching of its own.
	rebalanceLabSyncOnce(t, ctx, engine)

	trackedBefore, err := rebalanceLabTrackedFileCount(engine)(ctx)
	if err != nil {
		t.Fatalf("tracked file count after seeding: %v", err)
	}
	if trackedBefore != 3*perDisk {
		t.Fatalf("tracked file count = %d, want %d — junk files must not be tracked", trackedBefore, 3*perDisk)
	}

	// 40 of disk1's own 100 files: comfortably under RemovedFilesMax
	// (500), but 40/300 = 13.3%, over the default RemovedUpdatedPercent
	// (10%) — exactly what a single, unbatched sync would trip.
	const moveCount = 40
	var moves []RebalanceMove
	for i := 0; i < moveCount; i++ {
		rel := fmt.Sprintf("file%03d.bin", i)
		moves = append(moves, RebalanceMove{
			Share:        shareName,
			RelPath:      rel,
			SourceBranch: branches[0],
			TargetBranch: branches[1],
			Size:         int64(len(fmt.Sprintf("content-%03d", i))),
		})
	}
	plan := RebalancePlan{Moves: moves}

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Sync = rebalanceLabSyncFunc(engine)
	deps.TrackedFileCount = rebalanceLabTrackedFileCount(engine)

	var maxBatch int
	deps.RebalanceBatchLimit = func() int { return DefaultRebalanceBatchLimit }
	origSync := deps.Sync
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		if len(manifest) > maxBatch {
			maxBatch = len(manifest)
		}
		return origSync(ctx, manifest)
	}

	report, err := RunRebalance(ctx, plan, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance against the real, unmodified guard: %v", err)
	}
	if len(report.Moved()) != moveCount {
		t.Fatalf("expected all %d moves to complete, got %+v", moveCount, report.Entries)
	}
	if maxBatch >= moveCount {
		t.Fatalf("largest single sync manifest = %d entries, want less than %d — the whole plan must never go through in one sync", maxBatch, moveCount)
	}

	diff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after the batched rebalance: %v", err)
	}
	if diff.Added != 0 || diff.Removed != 0 || diff.Updated != 0 {
		t.Fatalf("Diff after the batched rebalance reported pending changes: %+v, want none", diff)
	}

	// The sibling half: an unrelated mass deletion of comparable size, in
	// the same array, in the same test window, still trips the same
	// unmodified guard — proving this isn't a guard that has gone quiet,
	// only a caller that now sizes its own batches correctly against it.
	for i := 0; i < moveCount; i++ {
		n := 2*perDisk + i
		if err := os.Remove(filepath.Join(branches[2], fmt.Sprintf("file%03d.bin", n))); err != nil {
			t.Fatalf("removing %s: %v", branches[2], err)
		}
	}

	ch, err := engine.Sync(ctx, parity.SyncOpts{})
	if ch != nil {
		t.Fatal("Sync after the unrelated mass deletion: got a non-nil progress channel — a real sync must not have started")
	}
	var blocked *parity.GuardBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Sync after the unrelated mass deletion: err = %v, want a *GuardBlockedError", err)
	}
	var tripped bool
	for _, trig := range blocked.Result.Triggers {
		if trig == parity.TriggerRemovedUpdatedPercent {
			tripped = true
		}
	}
	if !tripped {
		t.Fatalf("Sync after the unrelated mass deletion: Triggers = %v, want TriggerRemovedUpdatedPercent", blocked.Result.Triggers)
	}
}
