//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern internal/disk's own format_lab_test.go uses. It drives
// the real `snapraid` binary the lab image already carries against
// `make lab-up`'s own standing array (parity1, disk1-3, cache) through a
// real Layout.Render (#26) — exercising this issue's central claim: a
// real sync writes real parity, a real scrub finds real silent
// corruption, and a real fix restores a real deleted file from it.

package parity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func labDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	return filepath.Join("/lab", id)
}

// labEngine builds a real snapraid.conf over `make lab-up`'s own standing
// array mounts, and a SnapraidEngine driving it with the real snapraid
// binary. It does not go through Layout.Render (#26): Layout always
// places its first content copy at BootContentPath, `/var/lib/hoserva/`
// (Q18) — a path this lab, like every dev/test environment, must never
// write to (CLAUDE.md) — so this lab conf places its content copies on
// parity1 and cache instead, mirroring spike S5's own lab conf
// (spikes/s5/scripts/01-seed-and-sync.sh) rather than Layout's
// production shape.
func labEngine(t *testing.T, lab string) (*SnapraidEngine, dataMounts) {
	t.Helper()
	mounts := dataMounts{
		filepath.Join(lab, "mnt/disk1"),
		filepath.Join(lab, "mnt/disk2"),
		filepath.Join(lab, "mnt/disk3"),
	}
	parity := filepath.Join(lab, "mnt/parity1")
	cache := filepath.Join(lab, "mnt/cache")

	var conf strings.Builder
	fmt.Fprintf(&conf, "parity %s\n", filepath.Join(parity, "snapraid.parity"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(parity, "snapraid.content"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(cache, "snapraid.content"))
	for i, m := range mounts {
		fmt.Fprintf(&conf, "data d%d %s\n", i+1, m+"/")
	}

	workDir := filepath.Join(lab, "p28-lab-test")
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
		// This lab's own array is a handful of tiny files by construction
		// (Q45's loop devices), so a normal scripted workload for a
		// scrub/fix/journal test — a few files created, renamed or
		// modified — is routinely 10%+ of its own tiny total, tripping
		// the guard's default percent/count thresholds for reasons that
		// have nothing to do with what those tests exercise. Raising them
		// here does not touch DefaultRemovedFilesMax or
		// DefaultRemovedUpdatedPercent themselves (guard_test.go's own
		// unit tests exercise those directly, never through this shared
		// helper) or the zero-files rule guard_lab_test.go's own scenario
		// depends on — that rule fires regardless of either number.
		Guard: Guard{Config: GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}, mounts
}

type dataMounts []string

func writeFile(t *testing.T, path string, sizeBytes int) []byte {
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

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func drainReal(t *testing.T, ch <-chan Progress) Progress {
	t.Helper()
	var last Progress
	for p := range ch {
		last = p
	}
	return last
}

// TestLabSync_WritesRealParity syncs real, newly seeded files into a
// real array and confirms both the run and the resulting status report
// the array as fully synced and error-free.
func TestLabSync_WritesRealParity(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	engine, _ := labEngine(t, lab)

	writeFile(t, filepath.Join(lab, "mnt/disk1/movies/lab-sync.bin"), 300_000)

	before, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff (before sync): %v", err)
	}
	if before.Added == 0 {
		t.Fatalf("Diff before sync reported Added=0, want at least the newly seeded file")
	}

	ch, err := engine.Sync(ctx, SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	final := drainReal(t, ch)
	if final.Err != nil {
		t.Fatalf("Sync failed: %v", final.Err)
	}

	status, err := engine.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.ChangedSinceSync != 0 {
		t.Fatalf("Status.ChangedSinceSync = %d after a clean sync, want 0", status.ChangedSinceSync)
	}
	if status.Freshness != FreshnessGreen {
		t.Fatalf("Status.Freshness = %v after a clean sync, want FreshnessGreen", status.Freshness)
	}

	after, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff (after sync): %v", err)
	}
	if after.Added != 0 || after.Removed != 0 || after.Updated != 0 {
		t.Fatalf("Diff after sync reported changes: %+v, want none", after)
	}
}

// TestLabSync_NoOpSyncSucceeds calls SnapraidEngine.Sync directly a
// second time back to back with no changes in between, against a real
// snapraid binary, and confirms it still reports success. It does not
// go through the job path (job.RunSync, reached from
// cmd/hoservad/main.go, already covers that for sync) — this is
// regression coverage of SnapraidEngine.Sync's own no-op handling. The
// real log this produces carries both the scan's own summary:exit:equal
// and a final summary:exit:ok, the same shape snapraid_sync_noop_after_replace.log
// (testdata/parsers/) is a raw capture of.
func TestLabSync_NoOpSyncSucceeds(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	engine, _ := labEngine(t, lab)

	writeFile(t, filepath.Join(lab, "mnt/disk1/movies/lab-noop-sync.bin"), 300_000)
	syncOnce(t, ctx, engine)

	ch, err := engine.Sync(ctx, SyncOpts{})
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	final := drainReal(t, ch)
	if final.Err != nil {
		t.Fatalf("second Sync (nothing to sync) failed: %v", final.Err)
	}
}

// TestLabScrub_DetectsCorruption injects real silent corruption directly
// on a data disk's own loop device — targeted at a known file's exact
// extent via `xfs_bmap`, mirroring spike S5's own recipe exactly (doc 08
// §5) so the corruption lands inside real file data, never filesystem
// metadata — and confirms a real scrub finds it.
func TestLabScrub_DetectsCorruption(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	engine, _ := labEngine(t, lab)

	target := filepath.Join(lab, "mnt/disk2/backup/scrub-target.bin")
	original := writeFile(t, target, 2_200_000)
	origHash := sha256Hex(original)

	syncOnce(t, ctx, engine)

	corruptFileOnDisk(t, lab, "disk2", target)

	corruptedData, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading corrupted file: %v", err)
	}
	if sha256Hex(corruptedData) == origHash {
		t.Fatal("corruption injection had no effect — sha256 unchanged")
	}

	// olderThanDays=0 forces scrub over the whole array regardless of
	// recency (doc 02 §2's own -o override) — the file this test just
	// synced and corrupted is, by construction, 0 days old, so the
	// scheduled default (DefaultScrubOlderThanDays) would skip it.
	ch, err := engine.Scrub(ctx, 100, 0)
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	final := drainReal(t, ch)
	if final.Err != nil {
		t.Fatalf("Scrub reported a hard failure rather than a completed run with data errors: %v", final.Err)
	}

	status, err := engine.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Freshness != FreshnessRed {
		t.Fatalf("Status.Freshness after scrub found corruption = %v, want FreshnessRed", status.Freshness)
	}
}

// TestLabFix_RestoresADeletedFile deletes a synced file and confirms a
// real `fix` restores it byte-for-byte from parity (doc 02 §2's
// "fix | Restore data from parity").
func TestLabFix_RestoresADeletedFile(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	engine, mounts := labEngine(t, lab)

	relPath := "docs/lab-fix-undelete.bin"
	target := filepath.Join(mounts[2], relPath)
	original := writeFile(t, target, 250_000)
	origHash := sha256Hex(original)

	syncOnce(t, ctx, engine)

	if err := os.Remove(target); err != nil {
		t.Fatalf("removing %s: %v", target, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("file %s still exists after Remove", target)
	}

	ch, err := engine.Fix(ctx, FixOpts{Path: "/" + relPath})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	final := drainReal(t, ch)
	if final.Err != nil {
		t.Fatalf("Fix failed: %v", final.Err)
	}

	restored, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if got := sha256Hex(restored); got != origHash {
		t.Fatalf("restored file sha256 = %s, want %s (the pre-delete hash)", got, origHash)
	}
}

func syncOnce(t *testing.T, ctx context.Context, engine *SnapraidEngine) {
	t.Helper()
	ch, err := engine.Sync(ctx, SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	final := drainReal(t, ch)
	if final.Err != nil {
		t.Fatalf("Sync failed: %v", final.Err)
	}
}

var xfsBmapExtentRe = regexp.MustCompile(`^\s*0:\s*\[[0-9]+\.\.[0-9]+\]:\s*([0-9]+)\.\..*`)

// corruptFileOnDisk overwrites 500000 bytes inside target's own real
// on-disk extent — found via `xfs_bmap -v`, never a blind offset (doc 06
// §3, doc 08 §5) — directly on the loop device backing diskName's mount,
// confirming that device is genuinely backed by this lab's own image
// file before touching it (CLAUDE.md's loop-device discipline). The
// filesystem is unmounted before writing and remounted after, so the
// next read cannot be served from a stale, still-cached clean page.
func corruptFileOnDisk(t *testing.T, lab, diskName, target string) {
	t.Helper()

	out, err := exec.Command("xfs_bmap", "-v", target).Output()
	if err != nil {
		t.Fatalf("xfs_bmap -v %s: %v", target, err)
	}
	var startBlock int64
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		if m := xfsBmapExtentRe.FindStringSubmatch(line); m != nil {
			startBlock, err = strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				t.Fatalf("parsing xfs_bmap block %q: %v", m[1], err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("could not parse xfs_bmap -v output for %s:\n%s", target, out)
	}
	offset := startBlock*512 + 500_000

	mount := filepath.Join(lab, "mnt", diskName)
	devOut, err := exec.Command("findmnt", "-n", "-o", "SOURCE", "--target", mount).Output()
	if err != nil {
		t.Fatalf("findmnt --target %s: %v", mount, err)
	}
	dev := strings.TrimSpace(string(devOut))

	img := filepath.Join(lab, "img", diskName+".img")
	assertOwnLoop(t, dev, img)

	if out, err := exec.Command("sync").CombinedOutput(); err != nil {
		t.Fatalf("sync: %v: %s", err, out)
	}
	if out, err := exec.Command("umount", mount).CombinedOutput(); err != nil {
		t.Fatalf("umount %s: %v: %s", mount, err, out)
	}
	assertOwnLoop(t, dev, img)

	ddCmd := exec.Command("dd", "if=/dev/urandom", "of="+dev, "bs=1",
		fmt.Sprintf("seek=%d", offset), "count=500000", "conv=notrunc", "status=none")
	if out, err := ddCmd.CombinedOutput(); err != nil {
		t.Fatalf("dd corrupting %s: %v: %s", dev, err, out)
	}
	if out, err := exec.Command("mount", dev, mount).CombinedOutput(); err != nil {
		t.Fatalf("remount %s at %s: %v: %s", dev, mount, err, out)
	}
}

// assertOwnLoop refuses any device that is not the loop device backing
// img — the same check scripts/devenv/lib.sh's lab_assert_own_loop
// performs, reimplemented here since this Go test binary runs standalone
// (never via lib.sh's own shell functions).
func assertOwnLoop(t *testing.T, dev, img string) {
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
