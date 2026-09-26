package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
)

// storageTargetCommand is the `hoserva <command>` newArraySequence's own
// storage-target units carry in their doc 01 §2 header — there is no
// single user-facing command that triggers this regeneration (it runs
// automatically whenever seq is (re)built), so this names the closest
// surface a user actually has for the readiness this unit set gates.
const storageTargetCommand = "array status"

// storageTargetSync installs hoserva-storage.target, hoserva-storage-
// ready.service and every dependent service's drop-in (internal/pool/
// target.go, doc 02 §1, Q69) and keeps disk.StorageGate's readiness
// reflected in hoserva-storage-ready.service's own runtime flag
// (pool.StorageReadyFlagPath) — never by rewriting that unit's own
// content, and never by restarting a unit that is already correctly up
// or down. A rejected attempt at this fix restarted
// hoserva-storage-ready.service on every rebuild; systemd's own
// BindsTo=/Requires= (pool.ServiceDropIn) propagates a restart straight
// through to every dependent service (systemd.unit(5)), so a plain share
// create or disk-topology job stopped and restarted Samba, NFS, Docker
// and libvirt even while the array was already ready. One instance is
// shared for cmd/hoservad's whole lifetime, so Update can tell an
// unchanged rebuild from a real transition.
type storageTargetSync struct {
	Generator *cfggen.Generator
	Runner    disk.Runner
	// FlagPath overrides pool.StorageReadyFlagPath — set only by this
	// package's own tests, never in production.
	FlagPath string
	// PoolMounted overrides pool.IsMountedConfirmed — set only by this
	// package's own tests, never in production.
	PoolMounted func(path string) (bool, error)

	mu      sync.Mutex
	applied bool // false until Startup or Update has completed at least once
	ready   bool
	units   []string
}

func (s *storageTargetSync) poolMounted(path string) (bool, error) {
	if s.PoolMounted != nil {
		return s.PoolMounted(path)
	}
	return pool.IsMountedConfirmed(path)
}

// mountPool brings up seq's own mergerfs mounts — the catch-all and every
// share mount — through their own ArrayMount.Mount, the same mount-unit
// path ArraySequence.Start and RefreshLive already use (D1: never a new
// mount mechanism). Physical disk mounts are not included here: they are
// nofail and mount automatically the moment their own device appears
// (confirmed empirically against a real systemd), so by the time
// disk.StorageGate reports every expected disk present, seq.Disks are
// already mounted; the mergerfs layer over them is not tied to any device
// and never activates the same way.
func mountPool(ctx context.Context, seq *job.ArraySequence) error {
	if seq.CatchAll != nil {
		if err := seq.CatchAll.Mount(ctx); err != nil {
			return fmt.Errorf("mounting %s: %w", seq.CatchAll.Where(), err)
		}
	}
	for _, m := range seq.ShareMounts {
		if err := m.Mount(ctx); err != nil {
			return fmt.Errorf("mounting %s: %w", m.Where(), err)
		}
	}
	return nil
}

// confirmPoolMounted confirms the catch-all is genuinely a live mount —
// the check this issue exists for: disk.StorageGate.Ready() only reports
// every expected disk present by identity, never whether the pool that
// serves them is actually mounted, and a physical disk mount returning
// does not pull the mergerfs catch-all up with it (systemd.unit(5):
// RequiresMountsFor= only pulls the dependency in when the dependent
// itself starts, never the reverse — the identical one-directional gap
// pool.DependentServiceUnits has around hoserva-storage.target, confirmed
// the same way against a real systemd). It never mounts anything itself:
// ConfirmReady (below) calls it directly, after job.ArraySequence.Start's
// own Disks/CatchAll/ShareMounts loops have already mounted everything, so
// mounting a second time there would be redundant, not unsafe — that is
// what makes ConfirmReady's own name honest. mountAndConfirmPool (below)
// is this function's other caller, from Startup and Update, always
// immediately after mountPool has just mounted the same catch-all itself.
// Callers treat any error here exactly like disk.StorageGate
// itself reporting not ready — no flag, no dependent start: a gate
// confirmed only by disk identity must never be reported as if the array
// it gates were actually serving. seq.CatchAll == nil (no array
// configured yet) is not an error — there is nothing to confirm.
func (s *storageTargetSync) confirmPoolMounted(ctx context.Context, seq *job.ArraySequence) error {
	if seq.CatchAll == nil {
		return nil
	}
	mounted, err := s.poolMounted(seq.CatchAll.Where())
	if err != nil {
		return fmt.Errorf("confirming %s is mounted: %w", seq.CatchAll.Where(), err)
	}
	if !mounted {
		return fmt.Errorf("%s is not mounted", seq.CatchAll.Where())
	}
	return nil
}

// mountAndConfirmPool brings seq's own mergerfs mounts up (mountPool) and
// then confirms the catch-all actually is one (confirmPoolMounted).
// Startup (on an ordinary boot, outside maintenance mode), Update (a live
// transition while hoservad is already running, outside maintenance
// mode) and the explicit `array start` path
// (job.ArraySequence.Start, through ConfirmReady below, which mounts
// through Start's own Disks/CatchAll/ShareMounts loops instead of this
// function) all need the pool actually mounted before the gate can ever
// open — the generated pool mount units carry no [Install] section (D4),
// and at least the per-share mounts are not shown to come back on their
// own after a reboot (the nightly L3 suite's own reboot step, run
// 36226407069, found /mnt/user/massdel still unmounted while /mnt/user
// itself was already fuse.mergerfs); mounting the catch-all here too costs
// nothing whether or not something else also brings it up.
func (s *storageTargetSync) mountAndConfirmPool(ctx context.Context, seq *job.ArraySequence) error {
	if seq.CatchAll == nil {
		return nil
	}
	if err := mountPool(ctx, seq); err != nil {
		return err
	}
	return s.confirmPoolMounted(ctx, seq)
}

func (s *storageTargetSync) flagPath() string {
	if s.FlagPath != "" {
		return s.FlagPath
	}
	return pool.StorageReadyFlagPath
}

// clearFlag removes the runtime flag; an already-absent flag is success.
// Startup calls this unconditionally, before anything else, so a
// hoservad that dies before finishing this boot's own evaluation never
// leaves an earlier run's flag in force (doc 02 §1: the gate fails closed
// whenever hoservad has not itself confirmed readiness this boot).
func (s *storageTargetSync) clearFlag() error {
	if err := os.Remove(s.flagPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", s.flagPath(), err)
	}
	return nil
}

// setFlagReady creates the runtime flag, so hoserva-storage-ready.
// service's fixed `test -e` ExecStart starts succeeding.
func (s *storageTargetSync) setFlagReady() error {
	path := s.flagPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("ready\n"), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// diskMountUnitNames is every physical disk mount unit's file name seq
// carries, for hoserva-storage.target's own soft Wants=/After= ordering.
func diskMountUnitNames(seq *job.ArraySequence) []string {
	units := make([]string, 0, len(seq.Disks))
	for _, d := range seq.Disks {
		units = append(units, disk.UnitFileName(d.Where()))
	}
	return units
}

func gateReady(seq *job.ArraySequence) bool {
	return seq.Gate == nil || seq.Gate.Ready()
}

// writeUnits regenerates every storage-target unit from units and reloads
// systemd. Every file it writes is independent of readiness (D4:
// pool.StorageReadyUnit's own content never encodes
// disk.StorageGate.Ready()), so this never needs to run just because
// readiness alone changed.
func (s *storageTargetSync) writeUnits(ctx context.Context, units []string) error {
	if err := s.Generator.WriteStorageTarget(ctx, units, storageTargetCommand, 1, time.Now()); err != nil {
		return fmt.Errorf("writing storage-target units: %w", err)
	}
	if _, err := s.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload after writing storage-target units: %w", err)
	}
	return nil
}

// Startup installs seq's storage-target units and this boot's readiness
// flag, for cmd/hoservad's own startup call site. On an ordinary boot
// (outside maintenance mode) it brings the pool up itself —
// mountAndConfirmPool, the catch-all and every share mount — because the
// generated pool mount units carry no [Install] section (D4), and a
// physical disk's own nofail .mount unit activating on its own at boot
// never pulls the mergerfs layer over it up with it. An earlier version
// of this fix left that call out on the theory that Startup should only
// ever confirm what systemd itself already mounted; the nightly L3 reboot
// step (run 36226407069) caught the real consequence — every array disk
// mounted and /mnt/user itself already live, but /mnt/user/massdel stayed
// unmounted forever after a real reboot, since nothing else in the system
// is shown to bring a per-share mount back up on its own.
//
// It never issues a systemctl start or restart of a unit ordered
// After=hoserva.service (pool.HoservadServiceUnit) — hoserva-storage-
// ready.service is one such unit, so under packaging/debian/
// hoserva.service's own Type=notify, a blocking call to start or restart
// *that* here — before this very process has sent READY=1 — would
// deadlock waiting on a job systemd will not run until that notification
// arrives (reproduced empirically in the nightly L3 workflow, run
// 36197197961, against an earlier version of this fix that called
// systemctl restart from this call site). The pool's own mount units
// carry no such risk: they are not ordered after hoserva.service at all
// (confirmed against a real systemd in the session VM lab, 372-a1: a
// real reboot with a live array and share, hand-deployed hoservad from
// this fix, mounts /mnt/user and every share again and reaches
// hoserva.service active with no timeout, deadlock or restart loop).
// Nothing here starts hoserva-storage.target, hoserva-storage-
// ready.service or any of pool.DependentServiceUnits either way: each
// dependent service's own drop-in (BindsTo=hoserva-storage.target)
// already pulls the target in the moment systemd starts that service,
// which only happens once this call has returned and READY=1 has been
// sent.
//
// While the array is in maintenance mode (an explicit `array stop`,
// §4/Q70) this mounts nothing and leaves the gate closed instead, even
// with every disk present: a hoservad that restarts mid-swap must not
// remount storage the user explicitly took down. Maintenance mode is an
// in-memory Scheduler field, not persisted (#387), so this check only
// ever protects within one process's own lifetime — it does not by
// itself survive a hoservad restart; closing that gap is #387's own job.
//
// main.go calls notifySystemdReady regardless of whether this returns an
// error: withholding READY=1 buys no safety here (the flag is already
// cleared before this runs, so the gate is closed either way), and would
// instead put hoservad in a permanent kill-and-restart loop under
// Type=notify's own TimeoutStartSec, taking the API and UI down with it
// on every cycle. Whatever this returns, s.ready reflects the truth: the
// flag is left set only once mountAndConfirmPool has actually confirmed
// the pool is mounted, so a client can never be told the array is ready
// over an unconfirmed or unmounted pool.
func (s *storageTargetSync) Startup(ctx context.Context, seq *job.ArraySequence) error {
	if seq == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.clearFlag(); err != nil {
		return err
	}
	units := diskMountUnitNames(seq)
	if err := s.writeUnits(ctx, units); err != nil {
		return err
	}
	ready := gateReady(seq)
	switch {
	case !ready:
		// leave the flag cleared
	case seq.Scheduler != nil && seq.Scheduler.InMaintenance():
		log.Printf("hoservad: storage gate is ready but the array is in maintenance mode at startup — leaving Samba/NFS/Docker/libvirt gated closed until array start")
		ready = false
	default:
		if err := s.mountAndConfirmPool(ctx, seq); err != nil {
			return fmt.Errorf("mounting and confirming the pool before reporting the storage gate ready: %w", err)
		}
		if err := s.setFlagReady(); err != nil {
			return err
		}
	}
	s.applied, s.ready, s.units = true, ready, units
	return nil
}

// Update reflects a live topology or readiness change into the flag and,
// only on a not-ready→ready transition, starts every enabled, unmasked
// unit in pool.DependentServiceUnits (startDependents) — which, unlike a
// restart, pulls in a unit that is not yet active without touching one
// that already is (systemd.unit(5): "start" on an already-active unit is
// a no-op; confirmed against a real systemd, not just its docs — an
// already-active fake unit's own MainPID was unchanged across a repeat
// "start" in the lab this fix was built against). Starting the dependents themselves,
// not hoserva-storage.target, is what actually matters here: each
// dependent's own drop-in (pool.ServiceDropIn) declares BindsTo=
// hoserva-storage.target on *itself*, so starting it pulls the target in
// as a side effect — but the reverse edge does not exist in the
// generated units, so starting the target alone leaves an
// already-failed-or-never-started dependent exactly where it was
// (reproduced directly in the same lab: starting the target on its own
// left two fake BindsTo= dependents inactive; starting the dependents
// brought both them and the target active). For every other case Update
// never touches systemd at all: unchanged readiness and unchanged disk
// topology issue no systemctl call, and a ready→not-ready transition
// only removes the flag — this gate governs starting, never stopping, a
// service that is already correctly running (doc 02 §4's own explicit
// array-stop sequence is what stops them). Called only from after
// cmd/hoservad has already sent READY=1 (rebuildArraySequence:
// share.Service.PostCommit, every disk-topology job's ArrayReady hook,
// and the SIGHUP handler all run well after startup), so there is no
// ordering deadlock to avoid the way Startup has.
func (s *storageTargetSync) Update(ctx context.Context, seq *job.ArraySequence) {
	if seq == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	units := diskMountUnitNames(seq)
	ready := gateReady(seq)

	unitsChanged := !s.applied || !equalStringSlices(s.units, units)
	readyChanged := !s.applied || s.ready != ready

	if !unitsChanged && !readyChanged {
		return
	}

	if unitsChanged {
		if err := s.writeUnits(ctx, units); err != nil {
			log.Printf("hoservad: %v — Samba/NFS/Docker/libvirt boot ordering may be stale until this is retried", err)
		} else {
			s.units = units
			s.applied = true
		}
	}

	if !readyChanged {
		return
	}

	if !ready {
		if err := s.clearFlag(); err != nil {
			log.Printf("hoservad: clearing storage-ready flag: %v", err)
			return
		}
		s.ready, s.applied = false, true
		return
	}

	// A not-ready→ready transition while the array is in maintenance mode
	// (an explicit `array stop`, doc 02 §4/Q70) must not mount or serve
	// anything: ArraySequence.Stop already stopped Samba/NFS directly for
	// that, and every disk this gate cares about staying present is
	// exactly what an in-progress disk swap does not change. s.ready stays
	// false, so the next rebuild after the array starts again re-evaluates
	// this correctly instead of the transition being lost.
	if seq.Scheduler != nil && seq.Scheduler.InMaintenance() {
		log.Printf("hoservad: storage gate is ready but the array is in maintenance mode — leaving Samba/NFS/Docker/libvirt gated closed until array start")
		return
	}
	// mountAndConfirmPool is what makes this transition trustworthy:
	// disk.StorageGate.Ready() only reports every expected disk present by
	// identity, never whether the pool that serves them actually mounted,
	// and a physical disk returning does not pull the mergerfs catch-all
	// up with it. A failure here must be treated exactly like the gate
	// itself reporting not ready — no flag, no dependent start — or a
	// client could still write straight into the unmounted mountpoint on
	// the boot disk even though every unit this gate installs reports
	// active.
	if err := s.mountAndConfirmPool(ctx, seq); err != nil {
		log.Printf("hoservad: %v — leaving the storage-ready flag closed", err)
		return
	}
	if err := s.setFlagReady(); err != nil {
		log.Printf("hoservad: writing storage-ready flag: %v", err)
		return
	}
	s.startDependents(ctx)
	s.ready, s.applied = true, true
}

// startDependents starts every unit in pool.DependentServiceUnits that
// boot itself would have started — reusing disk.ServiceUnitController's
// own LoadState/UnitFileState rule (the same one ArraySequence.Services
// already applies to Samba and NFS) rather than an unconditional
// "systemctl start": a unit an admin disabled or masked on purpose (no
// VMs or Apps, SMB-only sharing, ...) must never be started back up just
// because the storage gate opened (#372 finding 5). Each dependent starts
// independently, and a failure is only ever logged, never fatal to the
// caller's own transition: Docker and libvirt are optional prerequisites
// (D8, doc 14) a given host may not have installed at all, and a
// "not found" for one of them must never stop smbd or nfs-kernel-server
// from starting, or leave the caller's own bookkeeping stuck retrying an
// expected-to-fail unit forever.
func (s *storageTargetSync) startDependents(ctx context.Context) {
	for _, svc := range pool.DependentServiceUnits {
		ctrl := disk.ServiceUnitController{ServiceName: svc, Unit: svc, Runner: s.Runner}
		if err := ctrl.Start(ctx); err != nil {
			log.Printf("hoservad: starting %s: %v", svc, err)
		}
	}
}

// ConfirmReady implements job.StorageTarget for job.ArraySequence.Start
// (#372 finding 1): an explicit `array start` is the user's own action to
// bring the array up, and Start calls this once its own Disks, CatchAll
// and ShareMounts are all mounted and confirmed — before it starts any
// ArrayService — so `systemctl start nfs-kernel-server.service` (that
// unit's own drop-in BindsTo=hoserva-storage.target) never hits a gate
// this boot's own evaluation left closed, the way it would after a
// not-ready boot, a data-disk upgrade's own reboot-then-resume flow (doc
// 02 §4 E4-E6), or a degraded boot followed by `array stop`/`array
// start`. Unlike Update, it never defers to maintenance mode: Start's own
// caller is still nominally "in maintenance" at the point this runs
// (Start only calls Scheduler.ExitMaintenance once every one of its own
// steps, including this one, has succeeded), so that is this
// transition's own starting point, not a reason to leave the gate
// closed. It never mounts the pool itself — Start's own Disks/CatchAll/
// ShareMounts loops, immediately before this call, already did — so it
// only confirms (confirmPoolMounted), never mounts.
func (s *storageTargetSync) ConfirmReady(ctx context.Context, seq *job.ArraySequence) error {
	if seq == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	units := diskMountUnitNames(seq)
	if err := s.writeUnits(ctx, units); err != nil {
		return err
	}
	if err := s.confirmPoolMounted(ctx, seq); err != nil {
		return fmt.Errorf("confirming the pool is mounted before starting dependent services: %w", err)
	}
	if err := s.setFlagReady(); err != nil {
		return err
	}
	s.startDependents(ctx)
	s.units, s.ready, s.applied = units, true, true
	return nil
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
