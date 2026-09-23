//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern disk_run_lab_test.go uses. It proves this issue's own
// wiring — TypeDiskAdd and TypeDiskReplace submitted through the real job
// scheduler — against real loop devices and a real snapraid binary,
// building on #32's already-lab-tested disk.FormatForAddition and the
// parity package's own already-lab-tested Fix/Check (lifecycle_lab_test.go):
// what is new here is that the job system's own RunDiskAdd/RunDiskReplace
// drive those primitives end to end, and that cancelling a replace mid-fix
// leaves the array recoverable through nothing more than an ordinary
// `hoserva fix`.

package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
)

func writeLabFile(t *testing.T, path string, size int) []byte {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return data
}

func sha256HexOfFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func drainRealProgress(t *testing.T, ch <-chan parity.Progress) parity.Progress {
	t.Helper()
	var final parity.Progress
	for p := range ch {
		final = p
	}
	return final
}

// resetBootContentDir clears and recreates parity.BootContentPath's own
// directory inside this lab container — never on the host, since this
// whole file only ever runs there (labDir requires HOSERVA_LAB_ID and the
// dispatch that sets it never runs this binary outside `docker compose
// exec`). Real snapraid syncs the job system's own generated config
// against this hardcoded path (Q18: "one copy on the boot device"), and a
// stale copy left by an earlier run of this same test would otherwise
// make a fresh array's own first sync see content history for a
// completely different layout.
func resetBootContentDir(t *testing.T) {
	t.Helper()
	dir := filepath.Dir(parity.BootContentPath)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("removing stale %s: %v", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// blkidTypeProbe reads dev's filesystem type by direct probe (`blkid -p`),
// bypassing whatever cached scan result blkidType's own plain `blkid`
// invocation would otherwise return. This lab container has no udev
// (doc 06 §3), so nothing invalidates blkid's cache when a loop device is
// detached and a different, freshly created image is immediately attached
// to the same, just-freed minor number (createLoopImage's own
// disk_run_lab_test.go cleanup runs `losetup -d` at the end of every
// test) — a plain `blkid` on that reused minor can report the *previous*
// occupant's filesystem for a device this test has confirmed, by reading
// its raw bytes directly, is genuinely still all zero. A refusal test's
// own "nothing was formatted" assertion needs the direct read, not the
// stale scan.
func blkidTypeProbe(ctx context.Context, r disk.Runner, dev string) string {
	out, _ := r.Run(ctx, "blkid", "-p", "-s", "TYPE", "-o", "value", dev)
	return strings.TrimSpace(string(out))
}

// assertOwnLoopDevice confirms dev is genuinely the loop device backing
// img — CLAUDE.md's own safety check before detaching anything, never
// losetup -D.
func assertOwnLoopDevice(t *testing.T, dev, img string) {
	t.Helper()
	out, err := exec.Command("losetup", "-j", img, "--output", "NAME", "--noheadings").Output()
	if err != nil || strings.TrimSpace(string(out)) != dev {
		t.Fatalf("losetup -j %s: got %q (err %v), want %q — refusing to detach a device this test did not attach", img, out, err, dev)
	}
}

// TestLabDiskAdd_JobWiringFormatsMountsAndRegeneratesConfig proves
// TypeDiskAdd, submitted through the real scheduler exactly as the addDisk
// handler would submit it, formats the new disk, mounts it at the next
// free /mnt/diskN, and regenerates snapraid.conf to include it — while an
// unrelated attached-but-unassigned loop device is never touched.
func TestLabDiskAdd_JobWiringFormatsMountsAndRegeneratesConfig(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	execRunner := disk.CommandRunner{}
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: execRunner}

	parityDev := createLoopImage(ctx, t, execRunner, lab, "p288-add-parity", "320M")
	data1Dev := createLoopImage(ctx, t, execRunner, lab, "p288-add-data1", "320M")
	cacheDev := createLoopImage(ctx, t, execRunner, lab, "p288-add-cache", "320M")
	newDev := createLoopImage(ctx, t, execRunner, lab, "p288-add-new", "320M")
	spareDev := createLoopImage(ctx, t, execRunner, lab, "p288-add-spare", "320M")
	loopSize := int64(320 << 20)

	t.Cleanup(func() {
		unmountIfMounted(execRunner, "/mnt/disk1")
		unmountIfMounted(execRunner, "/mnt/disk2")
		unmountIfMounted(execRunner, "/mnt/parity1")
		unmountIfMounted(execRunner, "/mnt/cache")
	})

	s := newTestScheduler(t)
	st, genRoot := labRegisterDiskFormat(t, s, provider, execRunner, disk.DirectMounter{Runner: execRunner})

	// Q18 needs parity-count+2 = 3 content-file copies on distinct
	// physical devices (boot, cache, data1) — a cache disk is what makes
	// a one-parity, one-data-disk array satisfiable at all.
	formatParams := validFormatParamsWithCache(parityDev, data1Dev, cacheDev, map[string]int64{parityDev: loopSize, data1Dev: loopSize, cacheDev: loopSize})
	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, formatParams))
	if err != nil {
		t.Fatalf("Submit(disk_format): %v", err)
	}
	if finished := await(t, s, j.ID); finished.Status != StatusSucceeded {
		t.Fatalf("disk_format status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	s.registry.Register(TypeDiskAdd, false, RunDiskAdd(DiskAddDeps{
		Provider:  provider,
		Runner:    execRunner,
		Store:     st,
		Generator: config.NewGenerator(genRoot),
		Mounter:   disk.DirectMounter{Runner: execRunner},
	}))

	newDisk := disk.AssignedDisk{Device: newDev, Filesystem: disk.XFS}
	addParams := DiskAddParams{
		Confirmation: SingleDiskConfirmation(newDisk),
		Disk:         newDisk,
		Sizes:        map[string]int64{parityDev: loopSize, data1Dev: loopSize, newDev: loopSize},
	}
	j2, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, addParams))
	if err != nil {
		t.Fatalf("Submit(disk_add): %v", err)
	}
	finished := await(t, s, j2.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("disk_add status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	if got := blkidType(ctx, execRunner, newDev); got != "xfs" {
		t.Fatalf("new disk %s: blkid TYPE = %q, want xfs", newDev, got)
	}
	if got := blkidType(ctx, execRunner, spareDev); got != "" {
		t.Fatalf("unrelated spare %s gained a filesystem (%q) — disk_add touched a disk it was never given", spareDev, got)
	}

	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	if len(disks) != 4 {
		t.Fatalf("len(disks) = %d, want 4 (parity + cache + 2 data)", len(disks))
	}
	added, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk2")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint(/mnt/disk2): %v", err)
	}
	if added.Device != newDev {
		t.Fatalf("added disk device = %q, want %q", added.Device, newDev)
	}

	newUUID := blkidUUID(ctx, execRunner, newDev)
	if got := findmntUUID(ctx, execRunner, "/mnt/disk2"); got != newUUID {
		t.Fatalf("/mnt/disk2 UUID = %q, want %q (the new disk)", got, newUUID)
	}
	if got := findmntUUID(ctx, execRunner, "/mnt/disk1"); got == "" {
		t.Fatal("/mnt/disk1 (already there before disk_add) is no longer mounted — disk_add disturbed the existing disk")
	}

	conf, err := os.ReadFile(filepath.Join(genRoot, "snapraid.conf"))
	if err != nil {
		t.Fatalf("reading snapraid.conf: %v", err)
	}
	if !strings.Contains(string(conf), "/mnt/disk2") {
		t.Fatalf("snapraid.conf missing the newly added disk:\n%s", conf)
	}
}

// TestLabDiskReplace_RefusesWhileSlotDiskStillMounted is finding 1's own
// lab regression test (doc 02 §4 steps 1-2): submitting disk_replace
// against a data disk that is still mounted — never actually unmounted or
// detached, a healthy disk rather than a failed one — must be refused
// before the replacement is formatted or the slot's row is touched, and
// the original disk must be left mounted and untouched.
func TestLabDiskReplace_RefusesWhileSlotDiskStillMounted(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	execRunner := disk.CommandRunner{}
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: execRunner}

	parityDev := createLoopImage(ctx, t, execRunner, lab, "p288-stillmounted-parity", "320M")
	data1Dev := createLoopImage(ctx, t, execRunner, lab, "p288-stillmounted-data1", "320M")
	data2Dev := createLoopImage(ctx, t, execRunner, lab, "p288-stillmounted-data2", "320M")
	loopSize := int64(320 << 20)

	t.Cleanup(func() {
		unmountIfMounted(execRunner, "/mnt/disk1")
		unmountIfMounted(execRunner, "/mnt/disk2")
		unmountIfMounted(execRunner, "/mnt/parity1")
	})

	s := newTestScheduler(t)
	st, genRoot := labRegisterDiskFormat(t, s, provider, execRunner, disk.DirectMounter{Runner: execRunner})

	formatParams := DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: parityDev, Filesystem: disk.XFS}},
		Data: []disk.AssignedDisk{
			{Device: data1Dev, Filesystem: disk.XFS},
			{Device: data2Dev, Filesystem: disk.XFS},
		},
		Sizes: map[string]int64{parityDev: loopSize, data1Dev: loopSize, data2Dev: loopSize},
	}
	formatParams.Confirmation = formatParams.Plan().Confirmation()
	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, formatParams))
	if err != nil {
		t.Fatalf("Submit(disk_format): %v", err)
	}
	if finished := await(t, s, j.ID); finished.Status != StatusSucceeded {
		t.Fatalf("disk_format status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	// data1Dev is deliberately left mounted — this test never unmounts or
	// detaches it, unlike TestLabDiskReplace_CancelledMidFixRecoversViaOrdinaryFix.
	replacementDev := createLoopImage(ctx, t, execRunner, lab, "p288-stillmounted-new", "320M")

	engine := &parity.SnapraidEngine{
		ConfPath: filepath.Join(genRoot, "snapraid.conf"),
		LogDir:   filepath.Join(genRoot, "snapraid-logs"),
		Runner:   parity.CommandRunner{},
		Guard:    parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}
	s.registry.Register(TypeDiskReplace, true, RunDiskReplace(DiskReplaceDeps{
		Provider:  provider,
		Runner:    execRunner,
		Store:     st,
		Generator: config.NewGenerator(genRoot),
		Mounter:   disk.DirectMounter{Runner: execRunner},
		Parity:    engine,
	}))

	replacement := disk.AssignedDisk{Device: replacementDev, Filesystem: disk.XFS}
	replaceParams := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk1",
		Disk:         replacement,
		Sizes:        map[string]int64{parityDev: loopSize, data2Dev: loopSize, replacementDev: loopSize},
	}
	rj, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, replaceParams))
	if err != nil {
		t.Fatalf("Submit(disk_replace): %v", err)
	}
	finished := await(t, s, rj.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("disk_replace status = %s, want failed — /mnt/disk1's own disk is still mounted (doc 02 §4)", finished.Status)
	}
	if !strings.Contains(finished.ErrorMessage, "still mounted") {
		t.Fatalf("ErrorMessage = %q, want a still-mounted refusal", finished.ErrorMessage)
	}

	if got := blkidTypeProbe(ctx, execRunner, replacementDev); got != "" {
		t.Fatalf("replacement %s gained a filesystem (%q) — a refused replace must format nothing", replacementDev, got)
	}

	unchanged, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint(/mnt/disk1): %v", err)
	}
	if unchanged.Device != data1Dev {
		t.Fatalf("disk1 device = %q after a refused replace, want unchanged %q", unchanged.Device, data1Dev)
	}
	if got := findmntUUID(ctx, execRunner, "/mnt/disk1"); got == "" {
		t.Fatal("/mnt/disk1 is no longer mounted after a refused replace — the original disk must be left alone")
	}
}

// TestLabDiskReplace_CancelledMidFixRecoversViaOrdinaryFix is this issue's
// own central safety-critical lab test (doc 02 §4 "Replacing a failed
// disk"): TypeDiskReplace, submitted through the real scheduler, formats
// the replacement and switches the array's own topology over to it before
// ever running snapraid fix; cancelling the job while a real `snapraid
// fix` subprocess is running (job.Scheduler.Cancel, which this issue
// registers TypeDiskReplace as cancellable for, since exec.CommandContext
// genuinely kills the subprocess) leaves that switch in place, and an
// ordinary, already-wired `hoserva fix` (TypeFix) against the same disk —
// no reformat, no second replace — completes the exact reconstruction the
// cancelled run did not finish. Throughout, an unrelated attached loop
// device is never touched.
func TestLabDiskReplace_CancelledMidFixRecoversViaOrdinaryFix(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	execRunner := disk.CommandRunner{}
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: execRunner}
	resetBootContentDir(t)

	parityDev := createLoopImage(ctx, t, execRunner, lab, "p288-replace-parity", "320M")
	data1Dev := createLoopImage(ctx, t, execRunner, lab, "p288-replace-data1", "320M")
	data2Dev := createLoopImage(ctx, t, execRunner, lab, "p288-replace-data2", "320M")
	spareDev := createLoopImage(ctx, t, execRunner, lab, "p288-replace-spare", "320M")
	loopSize := int64(320 << 20)

	t.Cleanup(func() {
		unmountIfMounted(execRunner, "/mnt/disk1")
		unmountIfMounted(execRunner, "/mnt/disk2")
		unmountIfMounted(execRunner, "/mnt/parity1")
	})

	s := newTestScheduler(t)
	st, genRoot := labRegisterDiskFormat(t, s, provider, execRunner, disk.DirectMounter{Runner: execRunner})

	formatParams := DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: parityDev, Filesystem: disk.XFS}},
		Data: []disk.AssignedDisk{
			{Device: data1Dev, Filesystem: disk.XFS},
			{Device: data2Dev, Filesystem: disk.XFS},
		},
		Sizes: map[string]int64{parityDev: loopSize, data1Dev: loopSize, data2Dev: loopSize},
	}
	formatParams.Confirmation = formatParams.Plan().Confirmation()
	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, formatParams))
	if err != nil {
		t.Fatalf("Submit(disk_format): %v", err)
	}
	if finished := await(t, s, j.ID); finished.Status != StatusSucceeded {
		t.Fatalf("disk_format status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	confPath := filepath.Join(genRoot, "snapraid.conf")
	engine := &parity.SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(genRoot, "snapraid-logs"),
		Runner:   parity.CommandRunner{},
		// This lab array is tiny by construction (a handful of scripted
		// files), which routinely exceeds the guard's production
		// thresholds for reasons unrelated to what this test exercises —
		// the same override lifecycle_lab_test.go's own labEngineWithMounts
		// uses.
		Guard: parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}

	// A real dataset large enough that a real `snapraid fix` over loop
	// devices takes measurably longer than formatting and persisting the
	// replacement's topology switch — the margin this test's own
	// mid-fix cancellation depends on.
	const fileCount = 40
	const fileSize = 1 << 20 // 1 MiB
	hashesBefore := make([]string, fileCount)
	for i := 0; i < fileCount; i++ {
		path := filepath.Join("/mnt/disk1", "docs", fmt.Sprintf("file%02d.bin", i))
		writeLabFile(t, path, fileSize)
		hashesBefore[i] = sha256HexOfFile(t, path)
	}
	keptOnDisk2 := filepath.Join("/mnt/disk2", "kept.bin")
	writeLabFile(t, keptOnDisk2, 500_000)
	disk2HashBefore := sha256HexOfFile(t, keptOnDisk2)

	syncCh, err := engine.Sync(ctx, parity.SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if final := drainRealProgress(t, syncCh); final.Err != nil {
		t.Fatalf("Sync failed: %v", final.Err)
	}

	// Fail disk1 for real: unmount and detach its own loop device — this
	// test's own image, never another's (CLAUDE.md: never losetup -D).
	if _, err := execRunner.Run(ctx, "sync"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := execRunner.Run(ctx, "umount", "/mnt/disk1"); err != nil {
		t.Fatalf("umount /mnt/disk1: %v", err)
	}
	img := filepath.Join(lab, "img", "p288-replace-data1.img")
	assertOwnLoopDevice(t, data1Dev, img)
	if _, err := execRunner.Run(ctx, "losetup", "-d", data1Dev); err != nil {
		t.Fatalf("losetup -d %s: %v", data1Dev, err)
	}

	replacementDev := createLoopImage(ctx, t, execRunner, lab, "p288-replace-new", "320M")

	s.registry.Register(TypeDiskReplace, true, RunDiskReplace(DiskReplaceDeps{
		Provider:  provider,
		Runner:    execRunner,
		Store:     st,
		Generator: config.NewGenerator(genRoot),
		Mounter:   disk.DirectMounter{Runner: execRunner},
		Parity:    engine,
	}))
	s.registry.Register(TypeFix, false, RunFix(engine))

	replacement := disk.AssignedDisk{Device: replacementDev, Filesystem: disk.XFS}
	replaceParams := DiskReplaceParams{
		Confirmation: SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk1",
		Disk:         replacement,
		Sizes:        map[string]int64{parityDev: loopSize, data2Dev: loopSize, replacementDev: loopSize},
	}
	rj, err := s.Submit(ctx, TypeDiskReplace, nil, mustJSON(t, replaceParams))
	if err != nil {
		t.Fatalf("Submit(disk_replace): %v", err)
	}

	// Long enough for FormatForAddition (a single mkfs.xfs on a 320M loop
	// device) and the topology switch to have certainly completed, short
	// enough that reconstructing 40 MiB through a real `snapraid fix`
	// has certainly not.
	time.Sleep(400 * time.Millisecond)
	if _, err := s.Cancel(ctx, rj.ID); err != nil {
		t.Fatalf("Cancel(%s): %v", rj.ID, err)
	}

	cancelled := await(t, s, rj.ID)
	t.Logf("disk_replace after Cancel: status=%s error=%q", cancelled.Status, cancelled.ErrorMessage)
	if cancelled.Status != StatusCancelled {
		t.Fatalf("disk_replace status = %s (%s), want cancelled — the mid-fix cancellation should have landed inside a still-running fix", cancelled.Status, cancelled.ErrorMessage)
	}

	if got := blkidType(ctx, execRunner, replacementDev); got != "xfs" {
		t.Fatalf("replacement %s: blkid TYPE = %q, want xfs — the topology switch step should have completed before cancellation", replacementDev, got)
	}
	if got := blkidType(ctx, execRunner, spareDev); got != "" {
		t.Fatalf("unrelated spare %s gained a filesystem (%q) — disk_replace touched a disk it was never given", spareDev, got)
	}
	switched, err := st.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint(/mnt/disk1) after cancellation: %v", err)
	}
	if switched.Device != replacementDev {
		t.Fatalf("disk1 device after cancellation = %q, want %q (topology should already be switched before the fix step ran)", switched.Device, replacementDev)
	}

	// Recovery: an ordinary, already-wired fix against the same disk —
	// not a second replace, not a reformat.
	fixParams := FixParams{Confirm: true, Disk: intPtr(1)}
	fj, err := s.Submit(ctx, TypeFix, nil, mustJSON(t, fixParams))
	if err != nil {
		t.Fatalf("Submit(fix): %v", err)
	}
	if finished := await(t, s, fj.ID); finished.Status != StatusSucceeded {
		t.Fatalf("recovery fix status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	for i := 0; i < fileCount; i++ {
		path := filepath.Join("/mnt/disk1", "docs", fmt.Sprintf("file%02d.bin", i))
		if got := sha256HexOfFile(t, path); got != hashesBefore[i] {
			t.Fatalf("restored %s sha256 = %s, want %s (pre-failure hash)", path, got, hashesBefore[i])
		}
	}
	if got := sha256HexOfFile(t, keptOnDisk2); got != disk2HashBefore {
		t.Fatalf("disk2's own file changed across the replace/fix cycle: got %s, want %s", got, disk2HashBefore)
	}
	if got := blkidType(ctx, execRunner, spareDev); got != "" {
		t.Fatalf("unrelated spare %s gained a filesystem (%q) after recovery", spareDev, got)
	}
}
