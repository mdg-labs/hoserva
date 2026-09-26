package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
)

// TestAcknowledgeDegraded_FlipsTheStorageReadyFlagAndStartsDependents is
// #385's own data-loss-adjacent scenario test, built the way main.go
// wires cmd/hoservad's production handler (safety-critical: a bypass here
// would start Samba, NFS, Docker and libvirt against a pool that was
// never actually remounted, doc 02 §1) — through wireAcknowledgeDegraded,
// the exact function main.go calls, never a hand copy of its closure.
// With an expected data disk missing, hoserva-storage-ready.service's own
// fixed `test -e pool.StorageReadyFlagPath` ExecStart (internal/pool's own
// StorageReadyUnit.Render, never rewritten by readiness — doc 02 §1) has
// nothing to find; acknowledging through the same production handler
// method the generated HTTP server calls (AcknowledgeDegradedArray) must
// make that same fixed command start succeeding, by way of the one
// transition #372 built for a returning disk (storageTargetSync's own
// not-ready→ready transition) — never a second mechanism, and never by
// rewriting the unit's content.
func TestAcknowledgeDegraded_FlipsTheStorageReadyFlagAndStartsDependents(t *testing.T) {
	ctx := context.Background()
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	scheduler := newTestScheduler(t)

	gate := disk.NewStorageGate([]disk.ExpectedDisk{
		{Identity: disk.Identity{Serial: "DATA1"}, Role: "data", MountAt: "/mnt/disk1"},
	})
	gate.Evaluate(nil) // the expected data disk is not present at boot
	if gate.Ready() {
		t.Fatal("gate.Ready() = true with the expected disk missing")
	}

	var catchAllCalls int32
	catchAll := storageTargetTestMount{where: "/mnt/user", calls: &catchAllCalls}
	seq := &job.ArraySequence{
		Gate:     job.PendingUpgradeGate{Gate: gate, Scheduler: scheduler},
		CatchAll: catchAll,
	}

	h := &api.Handler{}
	h.SetArray(seq)
	wireAcknowledgeDegraded(h, s, &acknowledgedDegraded{})

	if err := s.Startup(ctx, seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	// Before acknowledging: hoserva-storage-ready.service's own fixed
	// ExecStart (`test -e` StorageReadyFlagPath) has nothing to find, so
	// systemd would report it, and everything BindsTo= it, not ready.
	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag exists before the missing disk is acknowledged")
	}
	if catchAllCalls != 0 {
		t.Fatalf("catch-all Mount() called %d times before acknowledging, want 0 — the pool must not mount while the gate refuses readiness", catchAllCalls)
	}

	before, err := h.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !before.ArrayDegraded.Or(false) {
		t.Fatal("GetStatus.ArrayDegraded = false before acknowledging, with the expected disk missing")
	}
	if before.ArrayDegradedAcknowledged.Or(true) {
		t.Fatal("GetStatus.ArrayDegradedAcknowledged = true before acknowledging")
	}
	if before.StorageServicesReleased.Or(true) {
		t.Fatal("GetStatus.StorageServicesReleased = true before acknowledging")
	}

	status, err := h.AcknowledgeDegradedArray(ctx)
	if err != nil {
		t.Fatalf("AcknowledgeDegradedArray: %v", err)
	}

	// After acknowledging: the same fixed ExecStart's target now exists,
	// so hoserva-storage-ready.service (and hoserva-storage.target behind
	// it) would now report ready — the flag is the actual gate, per doc 02
	// §1's own design; the unit's content never changes.
	if _, err := os.Stat(s.flagPath()); err != nil {
		t.Fatalf("Stat(flag) = %v, want the readiness flag written once acknowledged", err)
	}
	if catchAllCalls != 1 {
		t.Fatalf("catch-all Mount() called %d times after acknowledging, want exactly 1 — acknowledging a missing disk must still mount and confirm the pool, never flip the flag on identity alone (doc 02 §1)", catchAllCalls)
	}
	fakeRunner := s.Runner.(*disk.FakeRunner)
	for _, svc := range pool.DependentServiceUnits {
		if !hasCall(fakeRunner.Calls(), "systemctl", "start", svc) {
			t.Fatalf("Calls() = %v, want a systemctl start %s once acknowledged", fakeRunner.Calls(), svc)
		}
	}
	// The disk is still physically missing — acknowledging must never
	// report the array as healthy (#385 finding 2). arrayDegraded stays
	// true; arrayDegradedAcknowledged is what flips, so the persistent
	// banner and top-bar pill can show "acknowledged, running degraded"
	// instead of clearing the warning outright.
	if !status.ArrayDegraded.Or(false) {
		t.Fatal("AcknowledgeDegradedArray's own returned status no longer reports arrayDegraded=true, even though the disk is still missing")
	}
	if !status.ArrayDegradedAcknowledged.Or(false) {
		t.Fatal("AcknowledgeDegradedArray's own returned status does not report the acknowledgement (arrayDegradedAcknowledged)")
	}
	if !status.StorageServicesReleased.Or(false) {
		t.Fatal("AcknowledgeDegradedArray's own returned status does not report storageServicesReleased=true once the transition actually started the gated services")
	}

	// A second acknowledge, with the disk still genuinely missing, is not
	// a fresh degraded state — disk.StorageGate.Acknowledge is idempotent
	// while nothing has been re-evaluated — but once the disk is found
	// present (a fresh Evaluate), a further acknowledge correctly refuses:
	// there is no longer anything degraded to acknowledge.
	gate.Evaluate([]disk.Identity{{Serial: "DATA1"}})
	if _, err := h.AcknowledgeDegradedArray(ctx); err == nil {
		t.Fatal("AcknowledgeDegradedArray = nil error once the missing disk is present again, want a refusal")
	}
}

// TestAcknowledgeDegraded_StopArrayReleasesStorageServicesReleased proves
// storageServicesReleased goes false the moment `array stop` enters
// maintenance mode, even though storageTargetSync's own readiness flag
// from an earlier acknowledgement stands until the missing disk actually
// reappears (finding 1): ArraySequence.Stop stops Samba and NFS and
// unmounts the pool without touching that flag, so GetStatus must derive
// storageServicesReleased from maintenance mode too, or a client would be
// told services are running while StopArray has just taken them down.
func TestAcknowledgeDegraded_StopArrayReleasesStorageServicesReleased(t *testing.T) {
	ctx := context.Background()
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	scheduler := newTestScheduler(t)

	gate := disk.NewStorageGate([]disk.ExpectedDisk{
		{Identity: disk.Identity{Serial: "DATA1"}, Role: "data", MountAt: "/mnt/disk1"},
	})
	gate.Evaluate(nil) // the expected data disk is not present at boot

	catchAll := storageTargetTestMount{where: "/mnt/user"}
	seq := &job.ArraySequence{
		Gate:      job.PendingUpgradeGate{Gate: gate, Scheduler: scheduler},
		CatchAll:  catchAll,
		Scheduler: scheduler,
	}

	h := &api.Handler{Scheduler: scheduler}
	h.SetArray(seq)
	wireAcknowledgeDegraded(h, s, &acknowledgedDegraded{})

	if err := s.Startup(ctx, seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if _, err := h.AcknowledgeDegradedArray(ctx); err != nil {
		t.Fatalf("AcknowledgeDegradedArray: %v", err)
	}
	acked, err := h.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus after acknowledging: %v", err)
	}
	if !acked.StorageServicesReleased.Or(false) {
		t.Fatal("GetStatus.StorageServicesReleased = false right after a successful acknowledge, want true")
	}

	if _, err := h.StopArray(ctx, confirmStop()); err != nil {
		t.Fatalf("StopArray: %v", err)
	}

	after, err := h.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus after StopArray: %v", err)
	}
	if after.StorageServicesReleased.Or(true) {
		t.Fatal("GetStatus.StorageServicesReleased = true after StopArray — the pool is unmounted and Samba/NFS are stopped, so the banner must not claim services are running")
	}
}

// TestAcknowledgeDegraded_RefusesWhenNothingIsMissing proves the handler
// never mounts anything or starts a dependent when there is no degraded
// state to acknowledge — a stale acknowledgement outliving the situation
// it was about would mask a real, future absence (disk.StorageGate.
// Acknowledge's own contract).
func TestAcknowledgeDegraded_RefusesWhenNothingIsMissing(t *testing.T) {
	ctx := context.Background()
	s := newTestStorageTargetSync(t)
	scheduler := newTestScheduler(t)

	gate := disk.NewStorageGate([]disk.ExpectedDisk{
		{Identity: disk.Identity{Serial: "DATA1"}, Role: "data", MountAt: "/mnt/disk1"},
	})
	gate.Evaluate([]disk.Identity{{Serial: "DATA1"}}) // every expected disk present
	if !gate.Ready() {
		t.Fatal("gate.Ready() = false with every expected disk present")
	}

	seq := &job.ArraySequence{Gate: job.PendingUpgradeGate{Gate: gate, Scheduler: scheduler}}
	h := &api.Handler{}
	h.SetArray(seq)
	wireAcknowledgeDegraded(h, s, &acknowledgedDegraded{})

	_, err := h.AcknowledgeDegradedArray(ctx)
	status := h.NewError(ctx, err)
	if status.StatusCode != 409 || status.Response.Code != "array_not_degraded" {
		t.Fatalf("AcknowledgeDegradedArray = %+v, want 409 array_not_degraded", status)
	}
	if calls := s.Runner.(*disk.FakeRunner).Calls(); len(calls) != 0 {
		t.Fatalf("Calls() = %v, want no systemctl call when there is nothing to acknowledge", calls)
	}
}

// TestAcknowledgeDegraded_MaintenanceModeReportsServicesNotStarted is
// finding 3's own regression test: acknowledging while the array is in
// maintenance mode (an explicit `array stop`, doc 02 §4/Q70) must not be
// reported as success — nothing actually mounted or started, since
// storageTargetSync's own transition defers to maintenance mode, and the
// caller has just promised the user services are starting.
func TestAcknowledgeDegraded_MaintenanceModeReportsServicesNotStarted(t *testing.T) {
	ctx := context.Background()
	s := newTestStorageTargetSync(t)
	scheduler := newTestScheduler(t)
	if err := scheduler.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	gate := disk.NewStorageGate([]disk.ExpectedDisk{
		{Identity: disk.Identity{Serial: "DATA1"}, Role: "data", MountAt: "/mnt/disk1"},
	})
	gate.Evaluate(nil)
	seq := &job.ArraySequence{Gate: job.PendingUpgradeGate{Gate: gate, Scheduler: scheduler}, Scheduler: scheduler}

	h := &api.Handler{}
	h.SetArray(seq)
	wireAcknowledgeDegraded(h, s, &acknowledgedDegraded{})

	_, err := h.AcknowledgeDegradedArray(ctx)
	status := handlerAPIError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "array_services_not_started" {
		t.Fatalf("AcknowledgeDegradedArray = %+v, want 409 array_services_not_started", status)
	}
	if _, statErr := os.Stat(s.flagPath()); statErr == nil {
		t.Fatal("the readiness flag exists even though the array is in maintenance mode")
	}
	// The acknowledgement itself still stands — only the transition that
	// was supposed to bring services up did not run.
	if !gate.Ready() {
		t.Fatal("gate.Ready() = false — the acknowledgement itself should still stand even though services did not start")
	}

	// #385 finding 2: a later GetStatus must report the acknowledgement
	// (arrayDegradedAcknowledged) as true — it still stands — while
	// storageServicesReleased stays false, since nothing was actually
	// started. A caller that read arrayDegradedAcknowledged alone would
	// wrongly tell the user services are running.
	after, err := h.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !after.ArrayDegradedAcknowledged.Or(false) {
		t.Fatal("GetStatus.ArrayDegradedAcknowledged = false after acknowledging in maintenance mode — the acknowledgement itself should still stand")
	}
	if after.StorageServicesReleased.Or(true) {
		t.Fatal("GetStatus.StorageServicesReleased = true after an acknowledge that refused with array_services_not_started — nothing was actually started")
	}
}

// TestAcknowledgeDegraded_MountFailureReportsServicesNotStarted is finding
// 3's other regression case: a failed catch-all mount during the
// not-ready→ready transition must also be reported to the caller, never
// silently swallowed as success — the web dialog and the CLI both promise
// services are starting, and a 200 here would leave Samba, NFS, Docker and
// libvirt down with no indication anything went wrong.
func TestAcknowledgeDegraded_MountFailureReportsServicesNotStarted(t *testing.T) {
	ctx := context.Background()
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	scheduler := newTestScheduler(t)

	gate := disk.NewStorageGate([]disk.ExpectedDisk{
		{Identity: disk.Identity{Serial: "DATA1"}, Role: "data", MountAt: "/mnt/disk1"},
	})
	gate.Evaluate(nil)
	catchAll := storageTargetTestMount{where: "/mnt/user", mountErr: errors.New("mount: /mnt/user: special device none does not exist")}
	seq := &job.ArraySequence{Gate: job.PendingUpgradeGate{Gate: gate, Scheduler: scheduler}, CatchAll: catchAll}

	h := &api.Handler{}
	h.SetArray(seq)
	wireAcknowledgeDegraded(h, s, &acknowledgedDegraded{})

	_, err := h.AcknowledgeDegradedArray(ctx)
	status := handlerAPIError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "array_services_not_started" {
		t.Fatalf("AcknowledgeDegradedArray = %+v, want 409 array_services_not_started", status)
	}
	if _, statErr := os.Stat(s.flagPath()); statErr == nil {
		t.Fatal("the readiness flag exists even though the pool failed to mount")
	}
	if !gate.Ready() {
		t.Fatal("gate.Ready() = false — the acknowledgement itself should still stand even though the pool failed to mount")
	}
}

// TestAcknowledgeDegraded_SurvivesEveryRebuild is finding 1's own
// regression test, reproduced through the real production path: it builds
// the daemon's array sequence with newArraySequence (a real, persisted
// array topology and a disk.Provider fake reporting one data disk
// missing), acknowledges through wireAcknowledgeDegraded, and then runs
// newRebuildArraySequence — the exact function main.go installs as
// shareService.PostCommit, the ArrayReady hook and the SIGHUP handler —
// to prove the acknowledgement survives it, rather than a rebuilt,
// never-acknowledged gate closing the readiness flag right back.
func TestAcknowledgeDegraded_SurvivesEveryRebuild(t *testing.T) {
	ctx, h, arrays, shares, provider, runner := newArrayTestEnv(t)
	disks := persistSampleArray(t, arrays)
	// disks[0] is the parity disk and stays present; disks[1] (DATA1) is
	// never added to provider, so it is the one disk missing across every
	// rebuild in this test.
	provider.AddDisk(disks[0].Device, disk.Disk{WWN: disks[0].WWN, Serial: disks[0].Serial, ByIDName: disks[0].ByIDName})
	scheduler := h.Scheduler

	seq, err := newArraySequence(ctx, scheduler, arrays, shares, provider, runner)
	if err != nil {
		t.Fatalf("newArraySequence: %v", err)
	}
	// Swap the real pool.MountController for arrayTestCatchAll (the same
	// substitution array_test.go's own wiring tests use) before anything
	// calls Mount() on it: production's own pool.SystemdMounter creates
	// its mountpoint directory for real (os.MkdirAll), and this initial
	// seq must never attempt that against a real /mnt path in a unit
	// test. The rebuilt seq newRebuildArraySequence itself later builds
	// still carries the real one — proving that path is what this test
	// exists to cover — but its own Update call below never reaches
	// mountAndConfirmPool at all, since neither disk topology nor
	// readiness actually changes between the two builds.
	realCatchAll, ok := seq.CatchAll.(pool.MountController)
	if !ok {
		t.Fatalf("CatchAll is %T, want pool.MountController", seq.CatchAll)
	}
	seq.CatchAll = arrayTestCatchAll{where: pool.CatchAllPath, argv: realCatchAll.Mnt.Argv(), runner: runner}
	h.SetArray(seq)

	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	if err := s.Startup(ctx, seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	ack := &acknowledgedDegraded{}
	wireAcknowledgeDegraded(h, s, ack)

	if _, err := h.AcknowledgeDegradedArray(ctx); err != nil {
		t.Fatalf("AcknowledgeDegradedArray: %v", err)
	}
	if _, err := os.Stat(s.flagPath()); err != nil {
		t.Fatalf("Stat(flag) = %v, want the readiness flag written once acknowledged", err)
	}

	// A share create, a disk-topology change or a SIGHUP all funnel
	// through this exact function (never a hand-copied rebuild) to
	// re-evaluate disk.StorageGate with DATA1 still the only disk absent.
	rebuild := newRebuildArraySequence(scheduler, arrays, shares, provider, runner, s, h, ack)
	if err := rebuild(ctx); err != nil {
		t.Fatalf("rebuildArraySequence: %v", err)
	}

	if _, err := os.Stat(s.flagPath()); err != nil {
		t.Fatalf("Stat(flag) = %v, want the readiness flag to survive a rebuild while DATA1 is still the only disk missing", err)
	}
	status, err := h.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !status.ArrayDegraded.Or(false) {
		t.Fatal("GetStatus.ArrayDegraded = false after a rebuild, want it to still report the missing disk")
	}
	if !status.ArrayDegradedAcknowledged.Or(false) {
		t.Fatal("GetStatus.ArrayDegradedAcknowledged = false after a rebuild — the earlier acknowledgement was lost")
	}
	gate, ok := storageGateOf(h.CurrentArray().Gate)
	if !ok {
		t.Fatal("h.CurrentArray().Gate is not the expected wrapper after rebuild")
	}
	if !gate.Ready() {
		t.Fatal("gate.Ready() = false after a rebuild that should have re-applied the earlier acknowledgement")
	}
}

// TestAcknowledgedDegraded_Reapply_RefusesWhenADifferentDiskIsAlsoMissing
// is finding 1's own explicit design contract: reapply only ever
// re-acknowledges the same degraded state, never a blanket "stay quiet
// forever" — a disk arriving, or a second, never-acknowledged disk also
// going missing, must surface as a fresh degraded state, not be masked by
// an earlier acknowledgement of an unrelated disk.
func TestAcknowledgedDegraded_Reapply_RefusesWhenADifferentDiskIsAlsoMissing(t *testing.T) {
	ack := &acknowledgedDegraded{}
	acknowledgedDisk := disk.ExpectedDisk{Identity: disk.Identity{Serial: "DATA1"}, Role: "data", MountAt: "/mnt/disk1"}
	ack.record([]disk.ExpectedDisk{acknowledgedDisk})

	gate := disk.NewStorageGate([]disk.ExpectedDisk{
		acknowledgedDisk,
		{Identity: disk.Identity{Serial: "DATA2"}, Role: "data", MountAt: "/mnt/disk2"},
	})
	// DATA1 (already acknowledged) and DATA2 (never acknowledged) are both
	// missing.
	gate.Evaluate(nil)

	ack.reapply(gate)
	if gate.Ready() {
		t.Fatal("reapply acknowledged a gate with a second, never-acknowledged disk also missing")
	}

	// DATA2 returns; DATA1 (the one this holder actually recorded) is still
	// the only one missing, so reapply now correctly re-acknowledges it.
	gate.Evaluate([]disk.Identity{{Serial: "DATA2"}})
	ack.reapply(gate)
	if !gate.Ready() {
		t.Fatal("reapply did not re-acknowledge a gate whose only missing disk is the one already recorded")
	}
}
