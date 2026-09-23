package job

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
)

// seedParityDiskArrayForUpgrade seeds one parity disk at parityMountpoint
// (a real, writable directory in every test that reaches Copying) plus
// two data disks at fixed, never-touched paths — enough distinct devices
// for Q18's content-placement rule once a completed upgrade's Release
// regenerates snapraid.conf.
func seedParityDiskArrayForUpgrade(t *testing.T, st *store.ArrayStore, parityMountpoint string) {
	t.Helper()
	err := st.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "20G",
		CreatedAt:    time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: parityMountpoint},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", Mountpoint: "/mnt/disk2"},
	})
	if err != nil {
		t.Fatalf("seedParityDiskArrayForUpgrade PutArray: %v", err)
	}
}

func registerDiskUpgradeParity(t *testing.T, s *Scheduler, p disk.Provider, r disk.Runner, st *store.ArrayStore, genRoot string, mounter, upgradeMounter disk.UnitMounter, eng parity.Engine) {
	t.Helper()
	s.registry.Register(TypeDiskUpgradeParity, true, RunDiskUpgradeParity(DiskUpgradeParityDeps{
		Provider:       p,
		Runner:         r,
		Store:          st,
		Generator:      config.NewGenerator(genRoot),
		Mounter:        mounter,
		UpgradeMounter: upgradeMounter,
		Parity:         eng,
		Now:            func() time.Time { return time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC) },
	}))
}

func TestRunDiskUpgradeParity_HappyPath(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	oldParity := t.TempDir()
	newParity := t.TempDir()

	original := []byte("parity-bytes-old")
	if err := os.WriteFile(filepath.Join(oldParity, "snapraid.parity"), original, 0o644); err != nil {
		t.Fatalf("seed old parity file: %v", err)
	}

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 16 * disk.TB})
	r := disk.NewFakeRunner()
	scriptFilesystemUUID(r, "/dev/sdz", "uuid-new")
	scriptMountedUUID(r, newParity, "uuid-new")

	seedParityDiskArrayForUpgrade(t, st, oldParity)

	eng := newRecordingEngine()
	eng.ScriptCheck([]parity.Progress{{Percent: 100}}, nil)

	mounter := disk.NewFakeMounter()
	registerDiskUpgradeParity(t, s, p, r, st, genRoot, mounter, mounter, eng)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	params := DiskUpgradeParityParams{
		Confirmation:  SingleDiskConfirmation(replacement),
		Mountpoint:    oldParity,
		NewMountpoint: newParity,
		Disk:          replacement,
		Sizes:         map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 2 * disk.TB, "/dev/sdc": 2 * disk.TB, "/dev/sdz": 16 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskUpgradeParity, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	if _, ok := p.FormattedAs("/dev/sdz"); !ok {
		t.Fatal("the new parity disk was never formatted")
	}

	settings, disks, gErr := st.GetArray(ctx)
	_ = settings
	if gErr != nil {
		t.Fatalf("GetArray: %v", gErr)
	}
	var found *store.ArrayDisk
	for i := range disks {
		if disks[i].Role == store.ArrayRoleParity {
			found = &disks[i]
		}
	}
	if found == nil {
		t.Fatal("no parity disk in the array after upgrade")
	}
	if found.Device != "/dev/sdz" || found.Mountpoint != newParity || found.FSUUID != "uuid-new" {
		t.Fatalf("upgraded parity row = %+v, want device=/dev/sdz mountpoint=%s", found, newParity)
	}

	// The old parity file's own bytes are untouched.
	oldBytes, err := os.ReadFile(filepath.Join(oldParity, "snapraid.parity"))
	if err != nil || string(oldBytes) != string(original) {
		t.Fatalf("old parity file after upgrade: data=%q err=%v, want unchanged", oldBytes, err)
	}
	newBytes, err := os.ReadFile(filepath.Join(newParity, "snapraid.parity"))
	if err != nil || string(newBytes) != string(original) {
		t.Fatalf("new parity file after upgrade: data=%q err=%v, want a byte-for-byte copy", newBytes, err)
	}

	// Release unmounts the old parity disk rather than only logging that it's released — left mounted, its
	// own mountpoint would stay occupied forever and a later upgrade's
	// NextParityMountpoint would reuse it while it's still the wrong
	// disk underneath.
	unmountedOld := false
	for _, u := range mounter.Unmounts {
		if u.Where == oldParity {
			unmountedOld = true
		}
	}
	if !unmountedOld {
		t.Fatalf("Release did not unmount the old parity disk at %s", oldParity)
	}
}

func TestRunDiskUpgradeParity_WrongConfirmationFormatsNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	oldParity := t.TempDir()
	newParity := t.TempDir()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 16 * disk.TB})
	seedParityDiskArrayForUpgrade(t, st, oldParity)

	eng := newRecordingEngine()
	mounter := disk.NewFakeMounter()
	registerDiskUpgradeParity(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter, mounter, eng)

	params := DiskUpgradeParityParams{
		Confirmation:  "wrong",
		Mountpoint:    oldParity,
		NewMountpoint: newParity,
		Disk:          disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS},
		Sizes:         map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdz": 16 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskUpgradeParity, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sdz"); ok {
		t.Fatal("wrong confirmation formatted the replacement disk anyway")
	}
}

func TestRunDiskUpgradeParity_UnknownMountpointRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	oldParity := t.TempDir()
	newParity := t.TempDir()

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 16 * disk.TB})
	seedParityDiskArrayForUpgrade(t, st, oldParity)

	eng := newRecordingEngine()
	mounter := disk.NewFakeMounter()
	registerDiskUpgradeParity(t, s, p, disk.NewFakeRunner(), st, genRoot, mounter, mounter, eng)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	params := DiskUpgradeParityParams{
		Confirmation:  SingleDiskConfirmation(replacement),
		Mountpoint:    "/mnt/does-not-exist",
		NewMountpoint: newParity,
		Disk:          replacement,
		Sizes:         map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdz": 16 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskUpgradeParity, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "no parity disk at") {
		t.Fatalf("ErrorMessage = %q, want a missing-slot refusal", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sdz"); ok {
		t.Fatal("an unknown mountpoint formatted the replacement disk anyway")
	}
}

// TestRunDiskUpgradeParity_ResumeSkipsFormatAndIdentityRecheck is this
// issue's own central resume-safety regression test for the parity flow:
// a resumed invocation (rc.InitialCheckpoint() non-empty) must neither
// re-list and re-validate identity nor reformat the new parity disk —
// doing either would refuse on the same now-expected FSUUID drift
// TestRunDiskUpgradeData_ResumeSkipsIdentityRecheckAndCompletes proves for
// the data flow, or — far worse — destroy whatever partial parity-file
// copy an earlier invocation already wrote.
func TestRunDiskUpgradeParity_ResumeSkipsFormatAndIdentityRecheck(t *testing.T) {
	ctx := context.Background()
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	oldParity := t.TempDir()
	newParity := t.TempDir()

	original := []byte("parity-bytes-old")
	if err := os.WriteFile(filepath.Join(oldParity, "snapraid.parity"), original, 0o644); err != nil {
		t.Fatalf("seed old parity file: %v", err)
	}
	// A resumed run's own copy phase starts from a checkpointed byte
	// offset, never from scratch — pre-seeding the new parity file with
	// the same content models "already fully copied by an earlier,
	// interrupted invocation" without needing to actually drive
	// RunParityUpgrade through Copying twice.
	if err := os.WriteFile(filepath.Join(newParity, "snapraid.parity"), original, 0o644); err != nil {
		t.Fatalf("seed new parity file: %v", err)
	}

	fakeProvider := disk.NewFakeProvider()
	fakeProvider.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	// No entry at all for /dev/sdz: a resumed invocation that (incorrectly)
	// re-lists and re-validates identity would find nothing here and
	// refuse — proving the assertion below is not vacuously true.
	countingProvider := &listCountingProvider{Provider: fakeProvider}
	formatCounting := &formatCountingProvider{Provider: countingProvider}

	r := disk.NewFakeRunner()
	scriptFilesystemUUID(r, "/dev/sdz", "uuid-new")
	scriptMountedUUID(r, newParity, "uuid-new")

	seedParityDiskArrayForUpgrade(t, st, oldParity)

	eng := newRecordingEngine()
	eng.ScriptCheck([]parity.Progress{{Percent: 100}}, nil)
	mounter := disk.NewFakeMounter()

	run := RunDiskUpgradeParity(DiskUpgradeParityDeps{
		Provider:       formatCounting,
		Runner:         r,
		Store:          st,
		Generator:      config.NewGenerator(genRoot),
		Mounter:        mounter,
		UpgradeMounter: mounter,
		Parity:         eng,
		Now:            func() time.Time { return time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC) },
	})

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	params := DiskUpgradeParityParams{
		Confirmation:  SingleDiskConfirmation(replacement),
		Mountpoint:    oldParity,
		NewMountpoint: newParity,
		Disk:          replacement,
		Sizes:         map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdz": 16 * disk.TB},
	}

	checkpoint := mustJSON(t, parity.ParityUpgradeCheckpoint{Phase: parity.ParityUpgradePhaseVerifying})
	var out bytes.Buffer
	rc := &RunContext{
		ctx:            ctx,
		checkpoint:     checkpoint,
		params:         mustJSON(t, params),
		out:            &out,
		stopRequested:  make(chan struct{}),
		saveCheckpoint: func([]byte) error { return nil },
		setProgress:    func(int) {},
	}

	if err := run(ctx, rc); err != nil {
		t.Fatalf("resumed RunDiskUpgradeParity: %v", err)
	}
	if calls := countingProvider.listCalls(); calls != 0 {
		t.Fatalf("Provider.List was called %d time(s) on a resumed invocation, want 0", calls)
	}
	if calls := formatCounting.formatCalls(); len(calls) != 0 {
		t.Fatalf("Format was called %v on a resumed invocation, want none — reformatting would destroy the already-copied parity file", calls)
	}

	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	var found *store.ArrayDisk
	for i := range disks {
		if disks[i].Role == store.ArrayRoleParity {
			found = &disks[i]
		}
	}
	if found == nil || found.Device != "/dev/sdz" || found.Mountpoint != newParity {
		t.Fatalf("upgraded parity row = %+v, want the switch to have completed", found)
	}
}

// TestRunDiskUpgradeParity_SecondUpgradeRefusesAStaleMountAtTheReusedSlot
// covers a stale mount at a reused slot: after one upgrade
// releases and unmounts its old parity disk, a second upgrade computing
// its own NewMountpoint from the store's now-freed slots (Q20's own
// dual-parity next step) can legitimately land back on that same path.
// If some other, unrelated disk is what is actually mounted there — the
// exact shape of the reported bug, a stale mount at a reused slot — this
// job must refuse before ever copying, verifying or switching the
// configuration onto it, rather than silently treating that wrong disk
// as the new parity disk.
func TestRunDiskUpgradeParity_SecondUpgradeRefusesAStaleMountAtTheReusedSlot(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	oldParity := t.TempDir()
	midParity := t.TempDir()

	if err := os.WriteFile(filepath.Join(oldParity, "snapraid.parity"), []byte("parity-v1"), 0o644); err != nil {
		t.Fatalf("seed old parity file: %v", err)
	}

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 16 * disk.TB})
	p.AddDisk("/dev/sdw", disk.Disk{Size: 24 * disk.TB})
	r := disk.NewFakeRunner()
	scriptFilesystemUUID(r, "/dev/sdz", "uuid-mid")
	scriptMountedUUID(r, midParity, "uuid-mid")
	scriptFilesystemUUID(r, "/dev/sdw", "uuid-second")

	seedParityDiskArrayForUpgrade(t, st, oldParity)

	eng := newRecordingEngine()
	eng.ScriptCheck([]parity.Progress{{Percent: 100}}, nil)
	mounter := disk.NewFakeMounter()
	registerDiskUpgradeParity(t, s, p, r, st, genRoot, mounter, mounter, eng)

	// First upgrade: releases and unmounts the original oldParity slot.
	first := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	firstParams := DiskUpgradeParityParams{
		Confirmation:  SingleDiskConfirmation(first),
		Mountpoint:    oldParity,
		NewMountpoint: midParity,
		Disk:          first,
		Sizes:         map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 2 * disk.TB, "/dev/sdc": 2 * disk.TB, "/dev/sdz": 16 * disk.TB},
	}
	j1, err := s.Submit(ctx, TypeDiskUpgradeParity, nil, mustJSON(t, firstParams))
	if err != nil {
		t.Fatalf("Submit(first): %v", err)
	}
	finished1 := await(t, s, j1.ID)
	if finished1.Status != StatusSucceeded {
		t.Fatalf("first upgrade status = %s (%s), want succeeded", finished1.Status, finished1.ErrorMessage)
	}

	// The second upgrade's own plan recomputes NextParityMountpoint
	// against the store's now-freed slots and lands back on oldParity —
	// but scripts findmnt there to report some other, unrelated disk's
	// UUID still mounted — a stale mount at a reused slot.
	scriptMountedUUID(r, oldParity, "uuid-stale-unrelated-disk")

	second := disk.AssignedDisk{Device: "/dev/sdw", Filesystem: disk.XFS}
	secondParams := DiskUpgradeParityParams{
		Confirmation:  SingleDiskConfirmation(second),
		Mountpoint:    midParity,
		NewMountpoint: oldParity,
		Disk:          second,
		Sizes:         map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 2 * disk.TB, "/dev/sdc": 2 * disk.TB, "/dev/sdw": 24 * disk.TB},
	}
	j2, err := s.Submit(ctx, TypeDiskUpgradeParity, nil, mustJSON(t, secondParams))
	if err != nil {
		t.Fatalf("Submit(second): %v", err)
	}
	finished2 := await(t, s, j2.ID)
	if finished2.Status != StatusFailed {
		t.Fatalf("second upgrade status = %s (%s), want failed — a stale mount at the reused slot must refuse, never be treated as the new disk", finished2.Status, finished2.ErrorMessage)
	}
	if !strings.Contains(finished2.ErrorMessage, "uuid-stale-unrelated-disk") {
		t.Fatalf("ErrorMessage = %q, want it to name the mismatched mounted UUID", finished2.ErrorMessage)
	}

	// Nothing was copied onto the stale disk still sitting at oldParity.
	got, err := os.ReadFile(filepath.Join(oldParity, "snapraid.parity"))
	if err != nil || string(got) != "parity-v1" {
		t.Fatalf("oldParity's own file after the refused second upgrade: data=%q err=%v, want unchanged \"parity-v1\"", got, err)
	}

	_, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	for _, d := range disks {
		if d.Role == store.ArrayRoleParity && d.Device != "/dev/sdz" {
			t.Fatalf("parity row switched to %+v after a refused second upgrade, want unchanged /dev/sdz", d)
		}
	}
}

// listCountingProvider records every List call so a resume test can prove
// a resumed parity upgrade never re-lists disk inventory.
type listCountingProvider struct {
	disk.Provider
	mu    sync.Mutex
	calls int
}

func (p *listCountingProvider) List(ctx context.Context) ([]disk.Disk, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return p.Provider.List(ctx)
}

func (p *listCountingProvider) listCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// runParityUpgradeResume drives one resumed RunDiskUpgradeParity
// invocation from checkpoint against a store whose parity slot has
// already been switched to newParity by the interrupted run.
func runParityUpgradeResume(t *testing.T, phase parity.ParityUpgradePhase) (*store.ArrayStore, *disk.FakeMounter, *recordingEngine, string, string, error) {
	t.Helper()
	ctx := context.Background()
	st := store.NewArrayStore(newTestDB(t))
	oldParity := t.TempDir()
	newParity := t.TempDir()
	original := []byte("parity-bytes-old")
	for _, dir := range []string{oldParity, newParity} {
		if err := os.WriteFile(filepath.Join(dir, "snapraid.parity"), original, 0o644); err != nil {
			t.Fatalf("seed parity file: %v", err)
		}
	}
	seedParityDiskArrayForUpgrade(t, st, oldParity)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS, WWN: "wwn-new"}
	// The interrupted run's ApplyLayout already committed the switch.
	if err := st.UpgradeParityDisk(ctx, oldParity, store.ArrayDisk{
		Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sdz", Filesystem: "xfs",
		FSUUID: "uuid-new", WWN: "wwn-new", Mountpoint: newParity,
	}); err != nil {
		t.Fatalf("UpgradeParityDisk: %v", err)
	}

	r := disk.NewFakeRunner()
	scriptFilesystemUUID(r, "/dev/sdz", "uuid-new")
	scriptMountedUUID(r, newParity, "uuid-new")
	eng := newRecordingEngine()
	eng.ScriptCheck([]parity.Progress{{Percent: 100}}, nil)
	mounter := disk.NewFakeMounter()

	run := RunDiskUpgradeParity(DiskUpgradeParityDeps{
		Provider:       disk.NewFakeProvider(),
		Runner:         r,
		Store:          st,
		Generator:      config.NewGenerator(t.TempDir()),
		Mounter:        mounter,
		UpgradeMounter: mounter,
		Parity:         eng,
		Now:            func() time.Time { return time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC) },
	})
	params := DiskUpgradeParityParams{
		Confirmation:  SingleDiskConfirmation(replacement),
		Mountpoint:    oldParity,
		NewMountpoint: newParity,
		Disk:          replacement,
		Sizes:         map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdz": 16 * disk.TB},
	}
	var out bytes.Buffer
	rc := &RunContext{
		ctx:            ctx,
		checkpoint:     mustJSON(t, parity.ParityUpgradeCheckpoint{Phase: phase}),
		params:         mustJSON(t, params),
		out:            &out,
		stopRequested:  make(chan struct{}),
		saveCheckpoint: func([]byte) error { return nil },
		setProgress:    func(int) {},
	}
	return st, mounter, eng, oldParity, newParity, run(ctx, rc)
}

// TestRunDiskUpgradeParity_ResumeAfterSwitchCompletes: an interruption
// after ApplyLayout moved the parity row to the new mountpoint — during
// `snapraid check`, or between the store commit and the checkpoint save —
// must resume through Checking and Release, not fail with "no parity disk"
// at the old mountpoint and leave the old parity disk mounted forever.
func TestRunDiskUpgradeParity_ResumeAfterSwitchCompletes(t *testing.T) {
	for _, phase := range []parity.ParityUpgradePhase{parity.ParityUpgradePhaseChecking, parity.ParityUpgradePhaseSwitchingConfig} {
		t.Run(string(phase), func(t *testing.T) {
			st, mounter, _, oldParity, newParity, err := runParityUpgradeResume(t, phase)
			if err != nil {
				t.Fatalf("resumed RunDiskUpgradeParity from %s: %v", phase, err)
			}
			released := false
			for _, u := range mounter.Unmounts {
				if u.Where == oldParity {
					released = true
				}
			}
			if !released {
				t.Fatalf("resume from %s never released the old parity disk at %s", phase, oldParity)
			}
			_, disks, err := st.GetArray(context.Background())
			if err != nil {
				t.Fatalf("GetArray: %v", err)
			}
			parityRows := 0
			for _, d := range disks {
				if d.Role == store.ArrayRoleParity {
					parityRows++
					if d.Mountpoint != newParity || d.Device != "/dev/sdz" {
						t.Fatalf("parity row = %+v, want /dev/sdz at %s", d, newParity)
					}
				}
			}
			if parityRows != 1 {
				t.Fatalf("parity rows = %d, want 1", parityRows)
			}
		})
	}
}

// TestRunDiskUpgradeParity_ResumeBeforeFirstCheckpointReformats: the first
// checkpoint is saved only after the copy completes, so a run interrupted
// after formatting resumes with an empty checkpoint and a disk whose
// filesystem UUID no longer matches the one submitted. That must not fail
// the job permanently as identity drift; the by-id identity is unchanged
// and the disk holds nothing but a partial parity copy.
func TestRunDiskUpgradeParity_ResumeBeforeFirstCheckpointReformats(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	st := store.NewArrayStore(newTestDB(t))
	oldParity := t.TempDir()
	newParity := t.TempDir()
	if err := os.WriteFile(filepath.Join(oldParity, "snapraid.parity"), []byte("parity-bytes-old"), 0o644); err != nil {
		t.Fatalf("seed old parity file: %v", err)
	}
	// A partial copy the interrupted run left behind.
	if err := os.WriteFile(filepath.Join(newParity, "snapraid.parity"), []byte("parity"), 0o644); err != nil {
		t.Fatalf("seed partial copy: %v", err)
	}

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 16 * disk.TB, WWN: "wwn-new", FSUUID: "uuid-from-the-interrupted-format"})
	r := disk.NewFakeRunner()
	scriptFilesystemUUID(r, "/dev/sdz", "uuid-new")
	scriptMountedUUID(r, newParity, "uuid-new")
	seedParityDiskArrayForUpgrade(t, st, oldParity)
	eng := newRecordingEngine()
	eng.ScriptCheck([]parity.Progress{{Percent: 100}}, nil)
	mounter := disk.NewFakeMounter()
	registerDiskUpgradeParity(t, s, p, r, st, t.TempDir(), mounter, mounter, eng)

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS, WWN: "wwn-new"}
	params := DiskUpgradeParityParams{
		Confirmation:  SingleDiskConfirmation(replacement),
		Mountpoint:    oldParity,
		NewMountpoint: newParity,
		Disk:          replacement,
		Sizes:         map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 2 * disk.TB, "/dev/sdc": 2 * disk.TB, "/dev/sdz": 16 * disk.TB},
	}
	j, err := s.Submit(ctx, TypeDiskUpgradeParity, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if finished := await(t, s, j.ID); finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
}
