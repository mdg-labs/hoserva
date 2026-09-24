//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern disk_run_lab_test.go and disk_lifecycle_lab_test.go use.
//
// This is #360's own data-loss regression test: it proves that once an
// earlier disk removal (#358, doc 09 §4 steps 6-8) has left a role_index
// gap, regenerating snapraid.conf keeps naming a surviving data disk by
// its own stable role_index, never by its new position in the array.
// #359 (the removal job itself) does not exist yet, so the gap is
// produced directly, the same way #358's own removal will leave it:
// empty the disk, run a real `sync -E` while it is still declared (the
// step SnapRAID itself requires before a disk can drop out of the
// config at all — confirmed empirically against a real snapraid binary;
// dropping the "data" line without it makes every later status/diff/sync
// refuse with "not present in the configuration file"), then delete its
// row from the store and regenerate — exactly the shape the issue
// describes.

package job

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
)

// TestLabRoleIndexGap_SurvivingDiskKeepsItsOwnNameAndRecovers is the
// acceptance criterion's data-loss scenario: a synced 3-data-disk array
// loses its middle disk's row, snapraid.conf is regenerated from what
// remains, and the surviving disk at the far slot (/mnt/disk3, role_index
// 3) must still be named "d3" — never renumbered to "d2" — so `snapraid
// diff` sees no change on it and `snapraid fix -d d3` still targets the
// right physical disk. A positional renderer (fmt.Sprintf("data d%d",
// i+1)) would instead call /mnt/disk3 "d2" here: SnapRAID's content file
// still records "d3" as /mnt/disk3's own file list, so a config that
// renamed it to "d2" would make diff report every one of disk3's real
// files as newly added under "d2" and every one of the actually-removed
// disk2's old files as removed under "d3" — exactly the silent
// data-loss shape this issue exists to prevent.
func TestLabRoleIndexGap_SurvivingDiskKeepsItsOwnNameAndRecovers(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	execRunner := disk.CommandRunner{}
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: execRunner}
	resetBootContentDir(t)

	parityDev := createLoopImage(ctx, t, execRunner, lab, "p360-gap-parity", "320M")
	cacheDev := createLoopImage(ctx, t, execRunner, lab, "p360-gap-cache", "320M")
	data1Dev := createLoopImage(ctx, t, execRunner, lab, "p360-gap-data1", "320M")
	data2Dev := createLoopImage(ctx, t, execRunner, lab, "p360-gap-data2", "320M")
	data3Dev := createLoopImage(ctx, t, execRunner, lab, "p360-gap-data3", "320M")
	loopSize := int64(320 << 20)

	t.Cleanup(func() {
		unmountIfMounted(execRunner, "/mnt/disk1")
		unmountIfMounted(execRunner, "/mnt/disk2")
		unmountIfMounted(execRunner, "/mnt/disk3")
		unmountIfMounted(execRunner, "/mnt/parity1")
		unmountIfMounted(execRunner, "/mnt/cache")
	})

	s := newTestScheduler(t)
	db := newTestDB(t)
	st := store.NewArrayStore(db)
	genRoot := t.TempDir()
	mounter := disk.DirectMounter{Runner: execRunner}
	labRegisterDiskFormatDeps(t, s, provider, execRunner, st, genRoot, mounter)

	// Q18's content-copy placement (parity+2 = 3 here) always fills the
	// boot device, then the cache disk, then the first data mount by
	// role_index order — so with a cache disk present, disk2 and disk3
	// never hold a content-file copy either before or after disk2's row
	// is removed. That keeps this test isolated to the "dN" naming
	// property #360 is about: without a cache disk, removing disk2 would
	// also shift which data mount is asked to hold the second content
	// copy (disk2 -> disk3), an orthogonal Q18 recompute #358 owns, not
	// this issue.
	cache := disk.AssignedDisk{Device: cacheDev, Filesystem: disk.XFS}
	formatParams := DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: parityDev, Filesystem: disk.XFS}},
		Data: []disk.AssignedDisk{
			{Device: data1Dev, Filesystem: disk.XFS},
			{Device: data2Dev, Filesystem: disk.XFS},
			{Device: data3Dev, Filesystem: disk.XFS},
		},
		Cache: &cache,
		Sizes: map[string]int64{parityDev: loopSize, cacheDev: loopSize, data1Dev: loopSize, data2Dev: loopSize, data3Dev: loopSize},
	}
	formatParams.Confirmation = formatParams.Plan().Confirmation()
	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, formatParams))
	if err != nil {
		t.Fatalf("Submit(disk_format): %v", err)
	}
	if finished := await(t, s, j.ID); finished.Status != StatusSucceeded {
		t.Fatalf("disk_format status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	writeLabFile(t, filepath.Join("/mnt/disk1", "a.bin"), 200_000)
	disk2File := filepath.Join("/mnt/disk2", "b.bin")
	writeLabFile(t, disk2File, 200_000)
	survivorPath := filepath.Join("/mnt/disk3", "c.bin")
	writeLabFile(t, survivorPath, 200_000)
	survivorHashBefore := sha256HexOfFile(t, survivorPath)

	confPath := filepath.Join(genRoot, "snapraid.conf")
	preRemoval, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("reading pre-removal snapraid.conf: %v", err)
	}
	for _, want := range []string{"data d1 /mnt/disk1/\n", "data d2 /mnt/disk2/\n", "data d3 /mnt/disk3/\n"} {
		if !strings.Contains(string(preRemoval), want) {
			t.Fatalf("snapraid.conf before removal = %q, want it to contain %q", preRemoval, want)
		}
	}

	engine := &parity.SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(genRoot, "snapraid-logs"),
		Runner:   parity.CommandRunner{},
		// This lab array is tiny by construction, which routinely exceeds
		// the guard's production thresholds for reasons unrelated to what
		// this test exercises — the same override
		// TestLabDiskReplace_CancelledMidFixRecoversViaOrdinaryFix uses.
		Guard: parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}
	syncCh, err := engine.Sync(ctx, parity.SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if final := drainRealProgress(t, syncCh); final.Err != nil {
		t.Fatalf("Sync failed: %v", final.Err)
	}

	// doc 09 §4 steps 6-8: empty the removing disk, then sync it away
	// while it is still declared. SnapRAID's own self-test refuses every
	// later status/diff/sync once a disk it has ever recorded vanishes
	// from the config without this step ("not present in the
	// configuration file! If you have removed it ... please restore
	// it") — confirmed against this lab's real snapraid binary. #359 (the
	// removal job) owns doing this through the job system; this test
	// reproduces its precondition directly via the same real binary,
	// through the still-unmodified 3-disk config.
	if err := os.Remove(disk2File); err != nil {
		t.Fatalf("removing %s: %v", disk2File, err)
	}
	if _, err := execRunner.Run(ctx, "snapraid", "-c", confPath, "sync", "-E"); err != nil {
		t.Fatalf("sync -E emptying disk2: %v", err)
	}

	// Now simulate #358's own removal shape: the middle data disk's row
	// is gone from the store, leaving role_index {1, 3} — #359 does not
	// exist yet, so this deletes the row directly rather than going
	// through a job.
	if _, err := db.ExecContext(ctx, "DELETE FROM array_disks WHERE mountpoint = ?", "/mnt/disk2"); err != nil {
		t.Fatalf("deleting disk2's row: %v", err)
	}
	if err := regenerateArrayFromStore(ctx, st, config.NewGenerator(genRoot), time.Now()); err != nil {
		t.Fatalf("regenerateArrayFromStore after removing disk2's row: %v", err)
	}

	regenerated, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("reading regenerated snapraid.conf: %v", err)
	}
	body := string(regenerated)
	for _, want := range []string{"data d1 /mnt/disk1/\n", "data d3 /mnt/disk3/\n"} {
		if !strings.Contains(body, want) {
			t.Fatalf("regenerated snapraid.conf = %q, want it to still contain %q", body, want)
		}
	}
	if strings.Contains(body, "data d2 ") {
		t.Fatalf("regenerated snapraid.conf = %q, must not renumber /mnt/disk3 down to d2", body)
	}

	// The test fails with positional naming: a renderer that names data
	// disks by position (fmt.Sprintf("data d%d", i+1)) would print
	// "data d2 /mnt/disk3/" here instead of "data d3 /mnt/disk3/", and the
	// assertion above would already have failed. What follows proves the
	// consequence a positional label would have had downstream: SnapRAID's
	// content file, unchanged since the sync above, still records "d3" as
	// /mnt/disk3's own file list — so a config that keeps calling it "d3"
	// sees no diff on it at all.
	report, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	for _, f := range report.RemovedFiles {
		if f.Disk == filepath.Clean("/mnt/disk3") {
			t.Fatalf("Diff reported a removal on /mnt/disk3 after an unrelated disk's row was dropped: %+v", f)
		}
	}
	for _, f := range report.AddedFiles {
		if f.Disk == filepath.Clean("/mnt/disk3") {
			t.Fatalf("Diff reported an addition on /mnt/disk3 after an unrelated disk's row was dropped: %+v", f)
		}
	}
	if diff, ok := report.PerDisk[filepath.Clean("/mnt/disk3")]; ok && diff.FilesBefore != diff.FilesAfter {
		t.Fatalf("PerDisk[/mnt/disk3] = %+v, want FilesBefore == FilesAfter (no change)", diff)
	}

	// Fail disk3 for real and recover it with an ordinary `fix -d 3` —
	// FixOptsFromParams maps that straight to "d3", the same label Render
	// just proved it kept.
	if err := os.Remove(survivorPath); err != nil {
		t.Fatalf("removing %s: %v", survivorPath, err)
	}

	s.registry.Register(TypeFix, false, RunFix(engine))
	fixParams := FixParams{Confirm: true, Disk: intPtr(3)}
	fj, err := s.Submit(ctx, TypeFix, nil, mustJSON(t, fixParams))
	if err != nil {
		t.Fatalf("Submit(fix): %v", err)
	}
	if finished := await(t, s, fj.ID); finished.Status != StatusSucceeded {
		t.Fatalf("fix -d 3 status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	if got := sha256HexOfFile(t, survivorPath); got != survivorHashBefore {
		t.Fatalf("restored %s sha256 = %s, want %s (pre-deletion hash) — fix -d 3 did not recover disk3's own file", survivorPath, got, survivorHashBefore)
	}
}
