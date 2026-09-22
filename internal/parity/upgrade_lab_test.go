//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45),
// never on the host: it is built with `go test -tags lab -c` from the
// host (compiling touches no device) and the resulting binary is run
// with `docker compose exec -T lab <binary>` inside the lab container,
// the same pattern snapraid_lab_test.go and lifecycle_lab_test.go use.
// It exercises this issue's own "Larger parity disk" acceptance
// criteria (doc 02 §4, Q71) against a real snapraid 12.4-1 binary: the
// parity file is copied, verified byte for byte, the configuration is
// switched, and a real `snapraid check` passes against the new parity
// disk before the old one is ever considered released — and killing
// the job mid-copy leaves the old parity file completely untouched.
//
// A dedicated pair of loop disks stands in for the old and new parity
// disks, and a dedicated fileset name keeps this test's own content and
// parity files independent of the standing array's own parity1
// (labEngine) and of every other lab test file in this package that
// shares disk1-3 — the same reasoning lifecycle_lab_test.go's own doc
// comment gives for doing the same thing.

package parity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// parityUpgradeLabEngine builds a real snapraid.conf naming parityMount
// as the sole parity disk and dataMounts as the data disks, with its
// own content-file copies kept off the parity disk entirely — one on
// cacheMount, one on dataMounts[0] — mirroring where Hoserva's own
// production Layout places them (doc 02 §2, Q18: never on the parity
// disk itself) far more closely than labEngine/labEngineWithMounts'
// own lab shortcut of putting a content copy on the parity mount,
// which this test's own parity-disk swap would otherwise orphan.
func parityUpgradeLabEngine(t *testing.T, confPath, filesetName, parityMount, cacheMount string, dataMounts []string) *SnapraidEngine {
	t.Helper()
	if err := os.WriteFile(confPath, []byte(parityUpgradeLabConfText(filesetName, parityMount, cacheMount, dataMounts)), 0o644); err != nil {
		t.Fatalf("writing %s: %v", confPath, err)
	}
	return &SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(filepath.Dir(confPath), "logs"),
		Runner:   CommandRunner{},
		// Same rationale as labEngine's own Guard override: this lab's
		// array is tiny by construction, so a handful of scripted file
		// changes routinely exceeds the guard's production thresholds
		// for reasons unrelated to what this test exercises.
		Guard: Guard{Config: GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}
}

// parityUpgradeLabConfText renders the snapraid.conf text naming
// parityMount as the sole parity disk, with content copies kept off it
// (see parityUpgradeLabEngine's own doc comment) — the same text both
// the initial config and RunParityUpgrade's own ApplyLayout hook (which
// calls this again with only parityMount changed) produce, so a real
// parity-disk upgrade's config regeneration is exercised as literally
// as this package's own lab tests can without internal/config's real
// generator (out of this issue's scope, D4).
func parityUpgradeLabConfText(filesetName, parityMount, cacheMount string, dataMounts []string) string {
	var conf strings.Builder
	fmt.Fprintf(&conf, "parity %s\n", filepath.Join(parityMount, filesetName+".parity"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(cacheMount, filesetName+".content"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(dataMounts[0], filesetName+".content"))
	for i, m := range dataMounts {
		fmt.Fprintf(&conf, "data d%d %s\n", i+1, m+"/")
	}
	return conf.String()
}

// createFormattedMountedLoopDisk truncates a fresh sparse image under
// lab's own img/ directory, attaches it, formats it XFS and mounts it
// at mountpoint — the same recipe create-array.sh and
// lifecycle_lab_test.go's own createAndMountLoopDisk use — detaching
// only this device, backed by the image this call created, once the
// test is done (CLAUDE.md: never losetup -D).
func createFormattedMountedLoopDisk(t *testing.T, lab, name, mountpoint, sizeMB string) string {
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
	t.Cleanup(func() {
		_, _ = exec.Command("umount", mountpoint).CombinedOutput()
		_, _ = exec.Command("losetup", "-d", dev).CombinedOutput()
	})
	return dev
}

// TestLabParityUpgrade_CopiesVerifiesSwitchesAndChecks is this issue's
// own "Larger parity disk" happy path (doc 02 §4, Q71): a real parity
// file, synced against real data disks, is copied to a second real
// loop disk, verified byte for byte, the configuration is switched to
// it, a real `snapraid check` passes, and the array's own diff stays
// clean throughout — the old parity disk's own file is never modified.
func TestLabParityUpgrade_CopiesVerifiesSwitchesAndChecks(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	_, mounts := labEngine(t, lab)
	cache := filepath.Join(lab, "mnt/cache")

	oldParity := filepath.Join(lab, "mnt", "p116-parity-old")
	createFormattedMountedLoopDisk(t, lab, "p116-parity-old", oldParity, "320M")
	newParity := filepath.Join(lab, "mnt", "p116-parity-new")
	createFormattedMountedLoopDisk(t, lab, "p116-parity-new", newParity, "340M")

	workDir := filepath.Join(lab, "p116-parity-upgrade")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", workDir, err)
	}
	confPath := filepath.Join(workDir, "snapraid.conf")

	engine := parityUpgradeLabEngine(t, confPath, "p116-upgrade", oldParity, cache, mounts)

	writeFile(t, filepath.Join(mounts[0], "p116/before-upgrade.bin"), 300_000)
	syncOnce(t, ctx, engine)

	beforeDiff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff before upgrade: %v", err)
	}
	if beforeDiff.Added != 0 || beforeDiff.Removed != 0 || beforeDiff.Updated != 0 {
		t.Fatalf("Diff before upgrade reported pending changes: %+v, want none", beforeDiff)
	}

	oldParityFile := filepath.Join(oldParity, "p116-upgrade.parity")
	newParityFile := filepath.Join(newParity, "p116-upgrade.parity")
	oldBytesBefore, err := os.ReadFile(oldParityFile)
	if err != nil {
		t.Fatalf("reading old parity file before upgrade: %v", err)
	}
	if len(oldBytesBefore) == 0 {
		t.Fatal("old parity file is empty after a real sync — nothing to copy")
	}

	var released bool
	deps := ParityUpgradeDeps{
		ApplyLayout: func(ctx context.Context, l Layout) error {
			return os.WriteFile(confPath, []byte(parityUpgradeLabConfText("p116-upgrade", newParity, cache, mounts)), 0o644)
		},
		Check: func(ctx context.Context, opts CheckOpts) (<-chan Progress, error) {
			return engine.Check(ctx, opts)
		},
		Release: func(ctx context.Context) error {
			released = true
			return nil
		},
	}
	spec := ParityUpgradeSpec{OldParityPath: oldParityFile, NewParityPath: newParityFile}

	result, err := RunParityUpgrade(ctx, spec, deps, ParityUpgradeHooks{}, nil)
	if err != nil {
		t.Fatalf("RunParityUpgrade: %v", err)
	}
	if result.Interrupted {
		t.Fatal("RunParityUpgrade reported Interrupted on a completed run")
	}
	if !released {
		t.Fatal("Release was never called after a successful upgrade")
	}

	oldBytesAfter, err := os.ReadFile(oldParityFile)
	if err != nil {
		t.Fatalf("reading old parity file after upgrade: %v", err)
	}
	if string(oldBytesAfter) != string(oldBytesBefore) {
		t.Fatal("old parity file's content changed during a successful upgrade")
	}
	newBytes, err := os.ReadFile(newParityFile)
	if err != nil {
		t.Fatalf("reading new parity file after upgrade: %v", err)
	}
	if string(newBytes) != string(oldBytesBefore) {
		t.Fatal("new parity file does not match the old one byte for byte")
	}

	// The configuration now names the new parity disk — confirmed by
	// reading it back through the same *SnapraidEngine and confPath,
	// exactly as a caller resuming after this run would.
	afterDiff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after switching to the new parity disk: %v", err)
	}
	if afterDiff.Added != 0 || afterDiff.Removed != 0 || afterDiff.Updated != 0 {
		t.Fatalf("Diff after switching parity disks reported changes: %+v, want none — protection must stay continuous", afterDiff)
	}

	// A further write, synced against the new parity disk, proves it is
	// genuinely live and writable, not merely a copy nobody uses.
	writeFile(t, filepath.Join(mounts[1], "p116/after-upgrade.bin"), 90_000)
	syncOnce(t, ctx, engine)
	finalDiff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after a post-upgrade sync: %v", err)
	}
	if finalDiff.Added != 0 || finalDiff.Removed != 0 || finalDiff.Updated != 0 {
		t.Fatalf("Diff after a post-upgrade sync reported changes: %+v, want none", finalDiff)
	}
}

// TestLabParityUpgrade_KilledMidCopy_OldParityFileUntouchedAndResumeCompletes
// is this issue's own central "kill the job at every step" test for the
// copying phase, against a real parity file: interrupting the copy
// partway through leaves the old parity file's real, on-disk content
// completely unchanged, and resuming finishes correctly with a real
// `snapraid check` passing against the new disk.
func TestLabParityUpgrade_KilledMidCopy_OldParityFileUntouchedAndResumeCompletes(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	_, mounts := labEngine(t, lab)
	cache := filepath.Join(lab, "mnt/cache")

	oldParity := filepath.Join(lab, "mnt", "p116-parity-kill-old")
	createFormattedMountedLoopDisk(t, lab, "p116-parity-kill-old", oldParity, "320M")
	newParity := filepath.Join(lab, "mnt", "p116-parity-kill-new")
	createFormattedMountedLoopDisk(t, lab, "p116-parity-kill-new", newParity, "340M")

	workDir := filepath.Join(lab, "p116-parity-upgrade-kill")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", workDir, err)
	}
	confPath := filepath.Join(workDir, "snapraid.conf")
	engine := parityUpgradeLabEngine(t, confPath, "p116-upgrade-kill", oldParity, cache, mounts)

	// A handful of real files, large enough that a real parity file
	// spans several of RunParityUpgrade's own small test-sized chunks.
	for i := 0; i < 4; i++ {
		writeFile(t, filepath.Join(mounts[2], "p116-kill", string(rune('a'+i))+".bin"), 700_000)
	}
	syncOnce(t, ctx, engine)

	oldParityFile := filepath.Join(oldParity, "p116-upgrade-kill.parity")
	newParityFile := filepath.Join(newParity, "p116-upgrade-kill.parity")
	oldBytesBefore, err := os.ReadFile(oldParityFile)
	if err != nil {
		t.Fatalf("reading old parity file: %v", err)
	}

	var released bool
	newDeps := func() ParityUpgradeDeps {
		return ParityUpgradeDeps{
			ChunkBytes: 32 * 1024,
			ApplyLayout: func(ctx context.Context, l Layout) error {
				return os.WriteFile(confPath, []byte(parityUpgradeLabConfText("p116-upgrade-kill", newParity, cache, mounts)), 0o644)
			},
			Check: func(ctx context.Context, opts CheckOpts) (<-chan Progress, error) {
				return engine.Check(ctx, opts)
			},
			Release: func(ctx context.Context) error {
				released = true
				return nil
			},
		}
	}
	spec := ParityUpgradeSpec{OldParityPath: oldParityFile, NewParityPath: newParityFile}

	stop := make(chan struct{})
	var logCalls int
	var savedCheckpoint []byte
	hooks := ParityUpgradeHooks{
		StopRequested: stop,
		SaveCheckpoint: func(data []byte) error {
			savedCheckpoint = append([]byte(nil), data...)
			return nil
		},
		Log: func(format string, args ...any) {
			logCalls++
			if logCalls == 2 {
				close(stop)
			}
		},
	}

	result, err := RunParityUpgrade(ctx, spec, newDeps(), hooks, nil)
	if err != nil {
		t.Fatalf("RunParityUpgrade (first, interrupted run): %v", err)
	}
	if !result.Interrupted {
		t.Fatal("RunParityUpgrade did not report Interrupted mid-copy")
	}
	if released {
		t.Fatal("Release was called before the copy even finished")
	}
	if string(mustReadFileLab(t, oldParityFile)) != string(oldBytesBefore) {
		t.Fatal("old parity file's real, on-disk content changed while the copy was interrupted")
	}
	if len(savedCheckpoint) == 0 {
		t.Fatal("no checkpoint was saved before the interruption")
	}

	result2, err := RunParityUpgrade(ctx, spec, newDeps(), ParityUpgradeHooks{}, savedCheckpoint)
	if err != nil {
		t.Fatalf("RunParityUpgrade (resumed run): %v", err)
	}
	if result2.Interrupted {
		t.Fatal("resumed run reported Interrupted")
	}
	if !released {
		t.Fatal("Release was never called after the resumed run completed")
	}
	if string(mustReadFileLab(t, oldParityFile)) != string(oldBytesBefore) {
		t.Fatal("old parity file's real, on-disk content changed after resuming and completing the upgrade")
	}
	if string(mustReadFileLab(t, newParityFile)) != string(oldBytesBefore) {
		t.Fatal("new parity file does not match the old one byte for byte after resuming")
	}
}

func mustReadFileLab(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}
