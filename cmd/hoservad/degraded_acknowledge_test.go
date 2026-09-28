package main

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

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
	rebuild := newRebuildArraySequence(scheduler, arrays, shares, provider, runner, s, nil, h, ack)
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

// TestAcknowledgeDegraded_SurvivesEveryRebuild_WrongFilesystem is #388
// finding 2's own regression test through the same real production path
// as TestAcknowledgeDegraded_SurvivesEveryRebuild, for the case that test
// does not cover: DATA1 is genuinely present the whole time — matched by
// identity, but formatted with a filesystem SQLite never recorded for
// this slot — so disk.StorageGate reports it through WrongFilesystem(),
// never Missing(). Before this fix, ack.record only ever read
// gate.Missing(), so this degraded state was silently dropped the moment
// any rebuild ran.
func TestAcknowledgeDegraded_SurvivesEveryRebuild_WrongFilesystem(t *testing.T) {
	ctx, h, arrays, shares, provider, runner := newArrayTestEnv(t)
	disks := persistSampleArray(t, arrays)
	// disks[0] (parity) is present and matches SQLite's own recorded
	// filesystem UUID exactly. disks[1] (DATA1) is present by identity too,
	// but carries a different filesystem UUID than SQLite recorded for its
	// slot — the #388 scenario — across every rebuild in this test.
	provider.AddDisk(disks[0].Device, disk.Disk{WWN: disks[0].WWN, Serial: disks[0].Serial, ByIDName: disks[0].ByIDName, FSUUID: disks[0].FSUUID})
	provider.AddDisk(disks[1].Device, disk.Disk{WWN: disks[1].WWN, Serial: disks[1].Serial, ByIDName: disks[1].ByIDName, FSUUID: "uuid-d-replaced"})
	scheduler := h.Scheduler

	seq, err := newArraySequence(ctx, scheduler, arrays, shares, provider, runner)
	if err != nil {
		t.Fatalf("newArraySequence: %v", err)
	}
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

	// A share create, a disk-topology change or a SIGHUP all funnel through
	// this exact function to re-evaluate disk.StorageGate with DATA1 still
	// the only wrong-filesystem slot.
	rebuild := newRebuildArraySequence(scheduler, arrays, shares, provider, runner, s, nil, h, ack)
	if err := rebuild(ctx); err != nil {
		t.Fatalf("rebuildArraySequence: %v", err)
	}

	if _, err := os.Stat(s.flagPath()); err != nil {
		t.Fatalf("Stat(flag) = %v, want the readiness flag to survive a rebuild while DATA1 is still the only wrong-filesystem disk — a rebuild runs on every udev block add, including an unrelated USB stick", err)
	}
	status, err := h.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !status.ArrayDegraded.Or(false) {
		t.Fatal("GetStatus.ArrayDegraded = false after a rebuild, want it to still report the wrong-filesystem disk")
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
	ack.record([]disk.ExpectedDisk{acknowledgedDisk}, nil)

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

// TestAcknowledgedDegraded_Reapply_SurvivesWrongFilesystemAcrossRebuild is
// #388 finding 2's own regression test: a wrong-filesystem slot (present
// by identity, but not Missing()) must be recorded and reapplied exactly
// like a missing one, since hoserva-storage.rules sends a rebuild on
// every udev block `add` — plugging in an unrelated USB stick is enough —
// and the daemon's own gate is rebuilt from scratch every time.
func TestAcknowledgedDegraded_Reapply_SurvivesWrongFilesystemAcrossRebuild(t *testing.T) {
	ack := &acknowledgedDegraded{}
	slot := disk.ExpectedDisk{Identity: disk.Identity{Serial: "DATA1", FSUUID: "uuid-original"}, Role: "data", MountAt: "/mnt/disk1"}
	gate := disk.NewStorageGate([]disk.ExpectedDisk{slot})
	gate.Evaluate([]disk.Identity{{Serial: "DATA1", FSUUID: "uuid-replaced"}})
	if gate.Ready() {
		t.Fatal("gate.Ready() = true with the only expected disk present but carrying the wrong filesystem")
	}
	if err := gate.Acknowledge(); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	ack.record(gate.Missing(), gate.WrongFilesystem())

	// A rebuild replaces the gate wholesale (newArraySequence): re-evaluate
	// a fresh one against the same still-mismatched disk.
	fresh := disk.NewStorageGate([]disk.ExpectedDisk{slot})
	fresh.Evaluate([]disk.Identity{{Serial: "DATA1", FSUUID: "uuid-replaced"}})
	ack.reapply(fresh)
	if !fresh.Ready() {
		t.Fatal("reapply did not re-acknowledge a gate whose only degraded slot is the wrong-filesystem one already recorded")
	}
}

// TestAcknowledgedDegraded_Reapply_ClearsOnceNothingIsDegraded proves the
// holder's own recorded set is dropped the moment a fresh Evaluate finds
// every expected disk present with the right filesystem — a stale
// acknowledgement must never survive to mask a later, genuinely new
// degraded slot with the same identity, the same rule
// disk.StorageGate.Evaluate already applies to its own acknowledged flag.
func TestAcknowledgedDegraded_Reapply_ClearsOnceNothingIsDegraded(t *testing.T) {
	ack := &acknowledgedDegraded{}
	slot := disk.ExpectedDisk{Identity: disk.Identity{Serial: "DATA1", FSUUID: "uuid-original"}, Role: "data", MountAt: "/mnt/disk1"}
	ack.record(nil, []disk.ExpectedDisk{slot})

	// The disk was reformatted back to the recorded filesystem — no longer
	// missing, no longer wrong-filesystem.
	gate := disk.NewStorageGate([]disk.ExpectedDisk{slot})
	gate.Evaluate([]disk.Identity{{Serial: "DATA1", FSUUID: "uuid-original"}})
	ack.reapply(gate)

	ack.mu.Lock()
	cleared := ack.identities == nil
	ack.mu.Unlock()
	if !cleared {
		t.Fatal("reapply did not clear the recorded set once the gate reported nothing degraded")
	}

	// A different disk going missing afterwards must surface as its own,
	// fresh degraded state — nothing from the cleared acknowledgement
	// should carry over.
	second := disk.ExpectedDisk{Identity: disk.Identity{Serial: "DATA2"}, Role: "data", MountAt: "/mnt/disk2"}
	gate2 := disk.NewStorageGate([]disk.ExpectedDisk{slot, second})
	gate2.Evaluate([]disk.Identity{{Serial: "DATA1", FSUUID: "uuid-original"}})
	ack.reapply(gate2)
	if gate2.Ready() {
		t.Fatal("reapply acknowledged a newly missing disk after its recorded set had already been cleared")
	}
}

// TestAcknowledgedDegraded_Reapply_RefusesWhenANewWrongFilesystemSlotAppears
// mirrors TestAcknowledgedDegraded_Reapply_RefusesWhenADifferentDiskIsAlso
// Missing for the wrong-filesystem category: an acknowledgement of one
// missing disk must not silently cover a second, different slot that
// later turns up present but with the wrong filesystem — that is its own,
// never-acknowledged degraded state.
func TestAcknowledgedDegraded_Reapply_RefusesWhenANewWrongFilesystemSlotAppears(t *testing.T) {
	ack := &acknowledgedDegraded{}
	acknowledgedDisk := disk.ExpectedDisk{Identity: disk.Identity{Serial: "DATA1"}, Role: "data", MountAt: "/mnt/disk1"}
	ack.record([]disk.ExpectedDisk{acknowledgedDisk}, nil)

	wrongFSDisk := disk.ExpectedDisk{Identity: disk.Identity{Serial: "DATA2", FSUUID: "uuid-original"}, Role: "data", MountAt: "/mnt/disk2"}
	gate := disk.NewStorageGate([]disk.ExpectedDisk{acknowledgedDisk, wrongFSDisk})
	// DATA1 (already acknowledged) is still missing; DATA2 is now present,
	// but carrying a filesystem never acknowledged for this slot.
	gate.Evaluate([]disk.Identity{{Serial: "DATA2", FSUUID: "uuid-replaced"}})

	ack.reapply(gate)
	if gate.Ready() {
		t.Fatal("reapply acknowledged a gate with a second, never-acknowledged wrong-filesystem slot")
	}
}

// TestAcknowledgedDegraded_Reapply_CoversSameIdentityFlippingCategory
// proves reapply's own identity match (disk.Identity.Matches) does not
// care which of Missing()/WrongFilesystem() a slot was in when it was
// acknowledged — the same physical disk going from "present, wrong
// filesystem" to "gone entirely" (or the reverse) is still the same
// acknowledged problem, not a fresh one.
func TestAcknowledgedDegraded_Reapply_CoversSameIdentityFlippingCategory(t *testing.T) {
	ack := &acknowledgedDegraded{}
	slot := disk.ExpectedDisk{Identity: disk.Identity{Serial: "DATA1", FSUUID: "uuid-original"}, Role: "data", MountAt: "/mnt/disk1"}
	// Acknowledged while present with the wrong filesystem.
	ack.record(nil, []disk.ExpectedDisk{slot})

	// The disk is pulled out entirely — same identity, now Missing()
	// instead of WrongFilesystem().
	gate := disk.NewStorageGate([]disk.ExpectedDisk{slot})
	gate.Evaluate(nil)
	ack.reapply(gate)
	if !gate.Ready() {
		t.Fatal("reapply did not cover the same identity's slot flipping from wrong-filesystem to missing")
	}
}

// blockingListProvider holds List until release is closed, once armed,
// so a test can park newRebuildArraySequence mid-rebuild.
type blockingListProvider struct {
	*disk.FakeProvider
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (p *blockingListProvider) List(ctx context.Context) ([]disk.Disk, error) {
	if p.armed.CompareAndSwap(true, false) {
		close(p.entered)
		<-p.release
	}
	return p.FakeProvider.List(ctx)
}

// TestAcknowledgeDegraded_WaitsForAnInFlightRebuild covers the
// acknowledge/rebuild interleaving (PR 394 review): a rebuild that
// re-applied acknowledgements onto its fresh gate before the hook
// recorded, then published that gate after the hook opened the storage
// target, would close the target again under an acknowledgement already
// reported as successful. The hook must wait for an in-flight rebuild and
// then acknowledge the gate that rebuild published.
func TestAcknowledgeDegraded_WaitsForAnInFlightRebuild(t *testing.T) {
	ctx, h, arrays, shares, fake, runner := newArrayTestEnv(t)
	disks := persistSampleArray(t, arrays)
	fake.AddDisk(disks[0].Device, disk.Disk{WWN: disks[0].WWN, Serial: disks[0].Serial, ByIDName: disks[0].ByIDName})
	provider := &blockingListProvider{FakeProvider: fake, entered: make(chan struct{}), release: make(chan struct{})}
	scheduler := h.Scheduler
	// Maintenance mode makes every storage-target transition below refuse
	// before it mounts anything, so neither sequence's real catch-all is
	// ever mounted in this unit test.
	if err := scheduler.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	seq, err := newArraySequence(ctx, scheduler, arrays, shares, provider, runner)
	if err != nil {
		t.Fatalf("newArraySequence: %v", err)
	}
	h.SetArray(seq)

	s := newTestStorageTargetSync(t)
	ack := &acknowledgedDegraded{}
	wireAcknowledgeDegraded(h, s, ack)
	rebuild := newRebuildArraySequence(scheduler, arrays, shares, provider, runner, s, nil, h, ack)

	provider.armed.Store(true)
	rebuildErr := make(chan error, 1)
	go func() { rebuildErr <- rebuild(ctx) }()
	<-provider.entered

	ackErr := make(chan error, 1)
	go func() {
		_, err := h.AcknowledgeDegradedArray(ctx)
		ackErr <- err
	}()
	select {
	case err := <-ackErr:
		close(provider.release)
		t.Fatalf("AcknowledgeDegradedArray returned (%v) while a rebuild was still in flight", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(provider.release)
	if err := <-rebuildErr; err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	select {
	case <-ackErr:
	case <-time.After(5 * time.Second):
		t.Fatal("AcknowledgeDegradedArray never returned once the rebuild finished")
	}

	rebuilt := h.CurrentArray()
	if rebuilt == seq {
		t.Fatal("the rebuild did not publish a new sequence")
	}
	gate, ok := storageGateOf(rebuilt.Gate)
	if !ok {
		t.Fatalf("rebuilt Gate is %T, want the storage gate wrapper", rebuilt.Gate)
	}
	if !gate.Ready() {
		t.Fatal("the rebuilt gate is not acknowledged — the acknowledgement landed on the gate the rebuild replaced")
	}
}

// TestNoBlankProbeOnStartupUpdateOrSighupRebuild is #398's own no-probe
// acceptance criterion, proved against cmd/hoservad's real production
// wiring: Startup, a not-ready→ready Update/UpdateOrError transition, and
// the SIGHUP rebuild (newRebuildArraySequence — the exact function
// installReloadHandler, shareService.PostCommit and every disk-topology
// job's ArrayReady hook all call, never a hand copy) must never reach for
// disk.BlankProber.ProbeBlank — reserved for the one device a replace
// request actually names, through job.ConfirmReplacementTargetAbsent —
// even for the scenario that might otherwise seem to invite it: a
// same-serial slot, present and matching SQLite's own recorded filesystem
// UUID, whose own physical-disk mount unit still fails to come up. A
// FakeBlankProber wired as handler.BlankProbe the same way main.go wires
// the real disk.LinuxBlankProber proves Probed() stays empty across every
// one of these calls; the single FakeRunner shared between storageTarget
// and every disk mount unit in this test proves it a second, independent
// way — no "blkid" argv of any kind reached it, not only the probe's own
// "-p" form, so a future regression that ran blkid directly from this
// path rather than through BlankProber would still be caught.
func TestNoBlankProbeOnStartupUpdateOrSighupRebuild(t *testing.T) {
	ctx, h, arrays, shares, provider, runner := newArrayTestEnv(t)
	disks := persistSampleArray(t, arrays)
	// Both disks are present, matched by identity and by SQLite's own
	// recorded filesystem UUID — disk.StorageGate reports this array fully
	// ready. Only DATA1's own physical mount unit is scripted to fail
	// below, so every mount attempt in this test bails out on it before
	// ever reaching the pool's own catch-all mount.
	provider.AddDisk(disks[0].Device, disk.Disk{WWN: disks[0].WWN, Serial: disks[0].Serial, ByIDName: disks[0].ByIDName, FSUUID: disks[0].FSUUID})
	provider.AddDisk(disks[1].Device, disk.Disk{WWN: disks[1].WWN, Serial: disks[1].Serial, ByIDName: disks[1].ByIDName, FSUUID: disks[1].FSUUID})

	seq, err := newArraySequence(ctx, h.Scheduler, arrays, shares, provider, runner)
	if err != nil {
		t.Fatalf("newArraySequence: %v", err)
	}
	h.SetArray(seq)

	runner.Script("systemctl", []string{"start", disk.UnitFileName(disks[1].Mountpoint)}, nil, errors.New("device dependency never resolved"))

	s := newTestStorageTargetSync(t)
	s.Runner = runner // the same Runner every disk mount unit in seq uses, so every call this test makes lands on one FakeRunner
	s.PoolMounted = func(string) (bool, error) { return true, nil }

	probe := disk.NewFakeBlankProber()
	h.BlankProbe = probe

	if err := s.Startup(ctx, seq); err == nil {
		t.Fatal("Startup with a failing disk mount = nil error, want one")
	}
	if failed := s.MountFailedMountpoints(); !failed[disks[1].Mountpoint] {
		t.Fatalf("MountFailedMountpoints() = %v, want %s recorded", failed, disks[1].Mountpoint)
	}

	s.Update(ctx, seq)
	if err := s.UpdateOrError(ctx, seq); err == nil {
		t.Fatal("UpdateOrError with a still-failing disk mount = nil error, want one")
	}

	ack := &acknowledgedDegraded{}
	rebuild := newRebuildArraySequence(h.Scheduler, arrays, shares, provider, runner, s, nil, h, ack)
	if err := rebuild(ctx); err != nil {
		t.Fatalf("rebuild (the SIGHUP path): %v", err)
	}

	if probed := probe.Probed(); len(probed) != 0 {
		t.Fatalf("BlankProber.Probed() = %v, want none — Startup, Update/UpdateOrError and the SIGHUP rebuild must never probe a device, even for a same-serial slot whose mount failed", probed)
	}
	for _, c := range runner.Calls() {
		if c.Name == "blkid" {
			t.Fatalf("unexpected blkid call during Startup/Update/rebuild: %+v — the replace-path probe must be the only caller of blkid -p, and List/GetPool/the rebuild/the timer must never open a device (Q13)", c)
		}
	}
}
