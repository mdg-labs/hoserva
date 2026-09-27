package main

import (
	"context"
	"errors"
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
	"github.com/mdg-labs/hoserva/internal/store"
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
	// StoppedFlagPath is disk.StorageStoppedFlagName inside --state-dir in
	// production (main.go sets it, and the same path on its Generator so
	// every unit's condition matches) and a temp path in every test (#387):
	// unlike FlagPath above, this has no silent fallback —
	// a test that forgot to set it would otherwise delete or write the
	// real host's array-stopped flag under /var/lib/hoserva the moment it
	// called Startup, Close or Open (CLAUDE.md: never touch that path
	// from a test).
	StoppedFlagPath string
	// ArrayStore and ShareStore, when set, let Startup regenerate every
	// managed disk and pool mount unit from SQLite before evaluating
	// readiness (#387) — nil only in this package's own tests,
	// which use job.ArraySequence fakes with nothing in SQLite to
	// regenerate from; production (main.go) always sets both.
	ArrayStore *store.ArrayStore
	ShareStore *store.ShareStore
	// PoolMounted overrides pool.IsMountedConfirmed — set only by this
	// package's own tests, never in production.
	PoolMounted func(path string) (bool, error)
	// ConfirmMountedUUID overrides disk.ConfirmMountedUUID — set only by
	// this package's own tests, never in production.
	ConfirmMountedUUID func(ctx context.Context, where, uuid string) error
	// StartupMountTimeout overrides startupMountTimeout — set only by
	// this package's own tests, never in production.
	StartupMountTimeout time.Duration

	mu      sync.Mutex
	applied bool // false until Startup or Update has completed at least once
	ready   bool
	units   []string

	// mountFailedMu guards mountFailedMountpoints on its own, deliberately
	// never s.mu (#398): Startup and updateTransition hold s.mu for their
	// whole bounded mount attempt (up to startupMountTimeout, a real
	// systemctl call under it) — a nightly L3 failure this fix closes had
	// GET /pool blocked behind exactly that hold, since MountFailedMount-
	// points used to share s.mu too. A caller that only ever wants the
	// last-known mount-failure snapshot (GetPool, api.Handler's own
	// MountFailedSlots hook) must never wait behind a live mount attempt
	// to read it.
	mountFailedMu sync.Mutex
	// mountFailedMountpoints records every mountpoint whose own physical
	// disk mount this sync's own bounded mount attempt (mountArrayDisks,
	// run from both Startup and updateTransition) most recently failed or
	// timed out to bring up (#398): the smallest honest signal GetPool
	// needs to tell a slot present by identity — disk.StorageGate itself
	// reports the array ready for it, since neither side's filesystem
	// UUID is positively known to differ, the same #388 fix that stops
	// hoservad's own restart loop — from one that is actually serving.
	// Reconciled against the live mount table (a stat, never a device
	// open — Q13) on every Startup and Update call, so a slot clears
	// itself the moment its disk actually mounts again — a successful
	// replace, or the physical swap coming back on its own — never only
	// on a restart.
	mountFailedMountpoints map[string]bool
}

func (s *storageTargetSync) poolMounted(path string) (bool, error) {
	if s.PoolMounted != nil {
		return s.PoolMounted(path)
	}
	return pool.IsMountedConfirmed(path)
}

func (s *storageTargetSync) confirmMountedUUID(ctx context.Context, where, uuid string) error {
	if s.ConfirmMountedUUID != nil {
		return s.ConfirmMountedUUID(ctx, where, uuid)
	}
	return disk.ConfirmMountedUUID(ctx, s.Runner, where, uuid)
}

// startupMountTimeout bounds every mount `systemctl start` Startup issues
// before main.go sends systemd's own READY=1 (#388): a disk that satisfies
// disk.StorageGate's identity check but carries the wrong filesystem is
// the case this bound exists for, but it holds regardless of *why* a mount
// unit's own device dependency never resolves — a generated `.mount` unit
// carries no JobTimeoutSec= of its own, so `systemctl start` otherwise
// waits on that job with no bound at all. Left unbounded, Type=notify's
// own TimeoutStartSec (default 90s, packaging/debian/hoserva.service)
// kills hoservad before Startup ever returns to send READY=1, and
// Restart=on-failure repeats the identical wait forever — the restart
// loop #388 reports. disk.Runner.Run execs through exec.CommandContext,
// so a timed-out context kills the systemctl client the moment this
// expires; the pending job it submitted may still be running in systemd
// itself, exactly as it would be if an operator had run the same command
// and given up on it, but Startup itself is unblocked and reports the
// mount as failed — treated exactly like the gate reporting not ready:
// no flag, no dependent start, and the next SIGHUP or rebuild retries it
// once the disk is actually fixed. 20s leaves ample headroom under the
// 90s default for every other step Startup and the rest of main.go's own
// startup path still need to run.
const startupMountTimeout = 20 * time.Second

func (s *storageTargetSync) startupMountTimeout() time.Duration {
	if s.StartupMountTimeout > 0 {
		return s.StartupMountTimeout
	}
	return startupMountTimeout
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

// mountArrayDisks brings every physical disk mount seq describes up
// through its own mount unit — Startup's own call, before mountPool
// (#387). Ordinarily nofail mounts a disk automatically the
// moment its own device appears, well ahead of this call; this exists for
// the boot where that never happened, because the array-stopped condition
// flag (disk.StorageStoppedFlagPath) was still set from before this
// boot's own reconciliation (already run above, ahead of this call) —
// that skips the disk's automatic activation silently, in the very same
// boot-time systemd transaction, and nothing else ever retries it.
// `systemctl start` on an already-active unit is a no-op (confirmed
// against a real systemd), so calling this on a disk that is already
// mounted costs nothing. failedWhere names the one mountpoint whose own
// Mount call actually failed or timed out (#398), so the caller can
// record it — this loop bails out on the first failure, so it is always
// at most one; a later mountpoint's own state is simply not yet known,
// never reported as failed.
func mountArrayDisks(ctx context.Context, seq *job.ArraySequence) (failedWhere string, err error) {
	for _, d := range seq.Disks {
		if err := d.Mount(ctx); err != nil {
			return d.Where(), fmt.Errorf("mounting %s: %w", d.Where(), err)
		}
	}
	return "", nil
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

// stoppedFlagPath returns s.StoppedFlagPath as set — main.go sets it to
// disk.StorageStoppedFlagName inside --state-dir, and every test sets it to
// a temp path (#387). Deliberately no fallback: a caller that
// forgot to set it gets an empty path, which os.WriteFile/os.Remove
// refuse outright, rather than silently defaulting to the real host path.
func (s *storageTargetSync) stoppedFlagPath() string {
	return s.StoppedFlagPath
}

// setStoppedFlag creates the array-stopped condition flag (#387): every
// generated mount unit carries ConditionPathExists=! this path, so a
// start any of them receives while it exists — including one an edge
// entirely outside Hoserva's own units reaches, like nfs-utils' own
// RequiresMountsFor= on nfs-server.service for an NFS-exported path — is
// skipped as a no-op rather than remounting a disk the user may be
// mid-swap on.
func (s *storageTargetSync) setStoppedFlag() error {
	path := s.stoppedFlagPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("stopped\n"), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// clearStoppedFlag removes the array-stopped condition flag; an
// already-absent flag is success. Called by Open (`array start`, before
// it mounts anything) and by Startup when persisted maintenance state
// says the array is not stopped.
func (s *storageTargetSync) clearStoppedFlag() error {
	if err := os.Remove(s.stoppedFlagPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", s.stoppedFlagPath(), err)
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

// Ready reports whether this sync's own not-ready→ready transition has
// actually run — the same s.ready this type already tracks internally to
// tell an unchanged rebuild from a real transition, exposed for
// api.Handler.StorageServicesReleased (#385). Unlike
// disk.StorageGate.Ready(), which flips the moment Acknowledge succeeds,
// this only ever reports true once mountAndConfirmPool has actually
// confirmed the pool and startDependents has run — never on the
// acknowledgement alone, so a caller can tell "acknowledged" from
// "acknowledged, and the gated services actually came up" apart.
func (s *storageTargetSync) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// recordMountFailure records where's own disk mount as failed (#398),
// under mountFailedMu — deliberately not s.mu (see that field's own doc
// comment). Safe to call regardless of what other lock, if any, the
// caller already holds.
func (s *storageTargetSync) recordMountFailure(where string) {
	if where == "" {
		return
	}
	s.mountFailedMu.Lock()
	defer s.mountFailedMu.Unlock()
	if s.mountFailedMountpoints == nil {
		s.mountFailedMountpoints = make(map[string]bool)
	}
	s.mountFailedMountpoints[where] = true
}

// expectedDiskUUIDs returns every array disk's own mountpoint and
// filesystem UUID from SQLite (doc 02 §4 UR4) — the same s.ArrayStore
// regenerateArrayMounts already reads, so reconcileMountFailures below can
// tell a slot's own disk apart from any other filesystem a manual mount or
// a leftover unit left at its mountpoint (#404). A nil s.ArrayStore
// (every test in this package but the ones that set it) reports no
// expected UUIDs at all — production (main.go) always sets it, and
// reconcileMountFailures falls back to a bare mount-presence check when
// no expected UUID is known for a given mountpoint, exactly like before
// this UUID check existed. ErrNoArray (topology deleted or never created)
// is not an error here: there is nothing to expect.
func (s *storageTargetSync) expectedDiskUUIDs(ctx context.Context) (map[string]string, error) {
	if s.ArrayStore == nil {
		return nil, nil
	}
	_, disks, err := s.ArrayStore.GetArray(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNoArray) {
			return nil, nil
		}
		return nil, err
	}
	uuids := make(map[string]string, len(disks))
	for _, d := range disks {
		uuids[d.Mountpoint] = d.FSUUID
	}
	return uuids, nil
}

// reconcileMountFailures drops every mountpoint currently tracked as
// failed (#398) that is now, in fact, a genuine mount of the slot's own
// disk — a stat of the live mount table (s.poolMounted, the same
// pool.IsMountedConfirmed stat mountAndConfirmPool itself uses) confirming
// something is mounted there, and, when this slot's own expected
// filesystem UUID is known (expectedDiskUUIDs), s.confirmMountedUUID
// confirming it is specifically that filesystem the mount table names
// (disk.ConfirmMountedUUID, findmnt — never a device open, Q13, doc 02 §4
// UR4) — so a slot self-heals the moment its own disk actually mounts
// again (a successful replace's own applyArrayFromStore, or the physical
// swap coming back on its own) rather than staying "needs attention"
// until the next restart, but a different filesystem left mounted at the
// same path by hand, or by a leftover unit, never clears it (#404). Runs
// under mountFailedMu, not s.mu (see that field's own doc comment). Cheap
// in the ordinary case: this only ever stats a mountpoint this sync
// itself previously recorded failed, typically none at all, and the
// SQLite read only runs when there is one.
func (s *storageTargetSync) reconcileMountFailures(ctx context.Context, seq *job.ArraySequence) {
	if seq == nil {
		return
	}
	s.mountFailedMu.Lock()
	defer s.mountFailedMu.Unlock()
	if len(s.mountFailedMountpoints) == 0 {
		return
	}
	// A store read that fails outright leaves every recorded failure
	// exactly as it is — never clear a slot on a mount check this couldn't
	// actually confirm.
	expected, err := s.expectedDiskUUIDs(ctx)
	if err != nil {
		return
	}
	for _, d := range seq.Disks {
		where := d.Where()
		if !s.mountFailedMountpoints[where] {
			continue
		}
		mounted, err := s.poolMounted(where)
		if err != nil || !mounted {
			continue
		}
		if uuid, ok := expected[where]; ok {
			if err := s.confirmMountedUUID(ctx, where, uuid); err != nil {
				continue
			}
		}
		delete(s.mountFailedMountpoints, where)
	}
}

// MountFailedMountpoints reports every mountpoint whose own disk mount
// this sync's own bounded mount attempt most recently failed or timed
// out to bring up (#398) — api.Handler's own MountFailedSlots hook reads
// it from a request goroutine. Guarded by mountFailedMu alone (see that
// field's own doc comment): this must never wait behind Startup or
// updateTransition's own s.mu hold across a live, possibly slow mount
// attempt just to read the last snapshot.
func (s *storageTargetSync) MountFailedMountpoints() map[string]bool {
	s.mountFailedMu.Lock()
	defer s.mountFailedMu.Unlock()
	out := make(map[string]bool, len(s.mountFailedMountpoints))
	for k := range s.mountFailedMountpoints {
		out[k] = true
	}
	return out
}

// mountArrayAndPool mounts seq's own physical disks (mountArrayDisks)
// and then its pool (mountAndConfirmPool), together under a single
// bounded deadline (#398, mirroring #388's own boot-time bound):
// Startup's boot case and updateTransition's live SIGHUP-arrival case
// both need the identical bound — a same-serial disk whose mount unit
// can never resolve its device must return control to the caller within
// startupMountTimeout, never hang on systemd's own default unit-start
// timeout (90s). Left unbounded on the updateTransition path, a nightly
// L3 run showed the concrete cost: installReloadHandler's own
// unconditional first rebuild (main.go, run before the API listeners
// ever start) called exactly this mount, and a same-serial blank
// replacement's stuck `systemctl start mnt-user.mount` — waiting via
// RequiresMountsFor= on the still-unmountable disk1 unit — kept the API
// listeners from ever starting within the 60s the disk-yank check
// allows. Any array-disk mount failure is recorded (recordMountFailure)
// regardless of which caller hit it, so GetPool can surface the slot
// mount_failed whether Startup or a later live rebuild is what actually
// ran into it. Callers must hold s.mu.
func (s *storageTargetSync) mountArrayAndPool(ctx context.Context, seq *job.ArraySequence) error {
	mountCtx, cancel := context.WithTimeout(ctx, s.startupMountTimeout())
	defer cancel()
	if failedWhere, err := mountArrayDisks(mountCtx, seq); err != nil {
		if failedWhere != "" {
			s.recordMountFailure(failedWhere)
		}
		return fmt.Errorf("mounting the array's own disks: %w", err)
	}
	if err := s.mountAndConfirmPool(mountCtx, seq); err != nil {
		return fmt.Errorf("mounting and confirming the pool: %w", err)
	}
	return nil
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
// remount storage the user explicitly took down. Maintenance mode is
// persisted in SQLite (#387) and restored by main.go's own call to
// Scheduler.RestorePersistedMaintenance before this runs, so this check
// holds across a crash, a restart, or a reboot, not only within the
// process that entered it.
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

	s.reconcileMountFailures(ctx, seq)
	if err := s.clearFlag(); err != nil {
		return err
	}
	// The array-stopped condition flag (#387) must reflect persisted
	// maintenance state before writeUnits below regenerates any mount
	// unit and before anything can race a start against it: a crash or
	// restart while the array was stopped this way must come back with
	// every generated mount unit still gated, not only the readiness
	// unit — RestorePersistedMaintenance (main.go) has already run by the
	// time this does, so InMaintenance() here reflects that persisted row,
	// not just this process's own in-memory history.
	inMaintenance := seq.Scheduler != nil && seq.Scheduler.InMaintenance()
	if inMaintenance {
		if err := s.setStoppedFlag(); err != nil {
			return err
		}
	} else if err := s.clearStoppedFlag(); err != nil {
		return err
	}
	// Regenerating every managed disk and pool mount unit from SQLite
	// (#387) runs here, after the array-stopped flag above is
	// already correct and before writeUnits below reloads systemd: an
	// array or a share created before ConditionPathExists=!
	// disk.StorageStoppedFlagPath existed in disk.MountUnit.Render/
	// pool.Mount.Render left its own unit condition-less forever, since
	// array creation, a share mutation, and the disk add/replace/upgrade
	// flows are the only other writers of those files, and none of them
	// runs again on a plain restart or package upgrade of an otherwise
	// untouched array.
	if err := s.regenerateArrayMounts(ctx); err != nil {
		return fmt.Errorf("regenerating disk and pool mount units from SQLite: %w", err)
	}
	units := diskMountUnitNames(seq)
	if err := s.writeUnits(ctx, units); err != nil {
		return err
	}
	ready := gateReady(seq)
	switch {
	case !ready:
		// leave the flag cleared
	case inMaintenance:
		log.Printf("hoservad: storage gate is ready but the array is in maintenance mode at startup — leaving Samba/NFS/Docker/libvirt gated closed until array start")
		ready = false
	default:
		// mountArrayAndPool (#387, bounded by #388/#398) runs mount-
		// ArrayDisks before mountAndConfirmPool: a boot where the array-
		// stopped flag was still set from before this boot's own
		// reconciliation above skipped every disk's automatic nofail
		// activation silently, in the very same boot-time systemd
		// transaction, and nothing else ever retries it — Startup must not
		// assume that activation happened just because the gate now reports
		// every expected disk present. Both mounts run under one bounded
		// deadline (see mountArrayAndPool's own doc comment for why this
		// cannot be left unbounded): the gate reporting ready by identity is
		// not a guarantee either actually mounts cleanly (a wrong-filesystem
		// slot is refused before it can reach here, evaluate/#388, but
		// nothing rules out a mount unit's own device dependency hanging
		// for some other reason). A mount that times out is returned as an
		// error exactly like one that fails outright: the switch above
		// already leaves the flag cleared and s.ready false, so main.go
		// still sends READY=1 on schedule and the next SIGHUP or rebuild
		// retries.
		if err := s.mountArrayAndPool(ctx, seq); err != nil {
			return fmt.Errorf("mounting the array's own disks and pool before reporting the storage gate ready: %w", err)
		}
		if err := s.setFlagReady(); err != nil {
			return err
		}
	}
	s.applied, s.ready, s.units = true, ready, units
	return nil
}

// regenerateArrayMounts calls job.RegenerateArrayMountsFromStore against
// s's own stores (#387). s.ArrayStore is nil only in this
// package's own tests, which build seq from fakes with nothing in SQLite
// to regenerate from; production (main.go) always sets both stores.
func (s *storageTargetSync) regenerateArrayMounts(ctx context.Context) error {
	if s.ArrayStore == nil {
		return nil
	}
	return job.RegenerateArrayMountsFromStore(ctx, s.ArrayStore, s.ShareStore, s.Generator, time.Now())
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
//
// Update only ever logs a transition that did not actually start
// anything (maintenance mode, or a mount/flag failure) — updateTransition
// carries the shared logic; UpdateOrError below is the same transition
// for a caller that needs to know it did not run (#385's AcknowledgeDegraded
// hook), rather than only see it logged.
func (s *storageTargetSync) Update(ctx context.Context, seq *job.ArraySequence) {
	if err := s.updateTransition(ctx, seq); err != nil {
		log.Printf("hoservad: %v", err)
	}
}

// UpdateOrError is Update's own not-ready→ready transition, reported to
// the caller instead of only logged (#385): AcknowledgeDegraded's
// own hook has just promised the user services are starting, so silently
// returning success while maintenance mode or a mount failure left every
// dependent exactly where it was would leave Samba, NFS, Docker and
// libvirt down with no indication anything went wrong.
func (s *storageTargetSync) UpdateOrError(ctx context.Context, seq *job.ArraySequence) error {
	return s.updateTransition(ctx, seq)
}

// errStorageTargetInMaintenance is updateTransition's own refusal when a
// not-ready→ready transition arrives while the array is in maintenance
// mode (an explicit `array stop`, doc 02 §4/Q70): ArraySequence.Stop
// already stopped Samba/NFS directly, and mounting or starting anything
// here over an in-progress disk swap would undo that.
var errStorageTargetInMaintenance = errors.New("storage gate is ready but the array is in maintenance mode — leaving Samba/NFS/Docker/libvirt gated closed until array start")

// updateTransition is Update and UpdateOrError's shared implementation;
// see Update's own doc comment above for the full contract.
func (s *storageTargetSync) updateTransition(ctx context.Context, seq *job.ArraySequence) error {
	if seq == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.reconcileMountFailures(ctx, seq)

	units := diskMountUnitNames(seq)
	ready := gateReady(seq)

	unitsChanged := !s.applied || !equalStringSlices(s.units, units)
	readyChanged := !s.applied || s.ready != ready

	if !unitsChanged && !readyChanged {
		return nil
	}

	if unitsChanged {
		// A refused write (a hand-edited managed unit) must fail this
		// transition, not just log it (#388, raised by the #385 executor):
		// UpdateOrError's whole point is telling a caller that surfaces an
		// outcome — the acknowledge operation — that boot ordering did not
		// actually update, rather than reporting success over stale units
		// on disk. Update's own caller still only ever logs it, exactly as
		// before, since it wraps this same call.
		if err := s.writeUnits(ctx, units); err != nil {
			return fmt.Errorf("%w — Samba/NFS/Docker/libvirt boot ordering may be stale until this is retried", err)
		}
		s.units = units
		s.applied = true
	}

	if !readyChanged {
		return nil
	}

	if !ready {
		if err := s.clearFlag(); err != nil {
			return fmt.Errorf("clearing storage-ready flag: %w", err)
		}
		s.ready, s.applied = false, true
		return nil
	}

	// A not-ready→ready transition while the array is in maintenance mode
	// (an explicit `array stop`, doc 02 §4/Q70) must not mount or serve
	// anything: ArraySequence.Stop already stopped Samba/NFS directly for
	// that, and every disk this gate cares about staying present is
	// exactly what an in-progress disk swap does not change. s.ready stays
	// false, so the next rebuild after the array starts again re-evaluates
	// this correctly instead of the transition being lost.
	if seq.Scheduler != nil && seq.Scheduler.InMaintenance() {
		return errStorageTargetInMaintenance
	}
	// mountArrayAndPool is what makes this transition trustworthy:
	// disk.StorageGate.Ready() only reports every expected disk present by
	// identity, never whether its own disks or the pool that serves them
	// actually mounted, and a physical disk returning does not pull the
	// mergerfs catch-all up with it. A same-serial disk newly arrived while
	// hoservad is already running (the SIGHUP path this transition serves)
	// needs its own array-disk mount attempted here exactly as Startup
	// attempts it at boot — Update never used to call mountArrayDisks at
	// all, so a mount_failed slot that only ever arrived live, never
	// present at the last boot, went unrecorded (#398). Bounded the same
	// way Startup bounds it (mountArrayAndPool's own doc comment): a
	// mount unit whose device dependency never resolves must return
	// control here within startupMountTimeout, not hang on systemd's own
	// default unit-start timeout — this call used to run on ctx directly,
	// unbounded, and a nightly L3 run showed the cost: installReloadHandler's
	// own unconditional first rebuild, run before main.go's API listeners
	// ever start, blocked here for the better part of systemd's own 90s
	// device timeout on a same-serial blank replacement. A failure here
	// must be treated exactly like the gate itself reporting not ready —
	// no flag, no dependent start — or a client could still write straight
	// into the unmounted mountpoint on the boot disk even though every
	// unit this gate installs reports active.
	if err := s.mountArrayAndPool(ctx, seq); err != nil {
		return fmt.Errorf("%w — leaving the storage-ready flag closed", err)
	}
	if err := s.setFlagReady(); err != nil {
		return fmt.Errorf("writing storage-ready flag: %w", err)
	}
	s.startDependents(ctx)
	s.ready, s.applied = true, true
	return nil
}

// startDependents starts every unit in pool.DependentServiceUnits that
// boot itself would have started — reusing disk.ServiceUnitController's
// own LoadState/UnitFileState rule (the same one ArraySequence.Services
// already applies to Samba and NFS) rather than an unconditional
// "systemctl start": a unit an admin disabled or masked on purpose (no
// VMs or Apps, SMB-only sharing, ...) must never be started back up just
// because the storage gate opened (#372). Each dependent starts
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
// (#372): an explicit `array start` is the user's own action to
// bring the array up, and Start calls this once its own Disks, CatchAll
// and ShareMounts are all mounted and confirmed — before it starts any
// ArrayService — so `systemctl start nfs-kernel-server.service` (that
// unit's own drop-in BindsTo=hoserva-storage.target) never hits a gate
// this boot's own evaluation left closed, the way it would after a
// not-ready boot, a data-disk upgrade's own reboot-then-resume flow (doc
// 02 §4 E4-E6), or a degraded boot followed by `array stop`/`array
// start`. Unlike Update, it never defers to maintenance mode: Start's own
// caller is still nominally "in maintenance" at the point this runs
// (Start only calls Scheduler.ExitMaintenanceChecked once every one of
// its own steps, including this one, has succeeded), so that is this
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

// dependentServicesOutsideArraySequence is pool.DependentServiceUnits
// without Samba and NFS: those two already stop and start through
// job.ArraySequence's own Services list (#309, doc 02 §4's "Samba and NFS
// stop"/"start" step). Docker and libvirt read the pool the same way
// (#387, doc 02 §1, Q70) but were never added to that list, so Close
// below is their only stop.
var dependentServicesOutsideArraySequence = []string{"docker.service", "libvirtd.service"}

// Close implements job.StorageTarget for job.ArraySequence.Stop, and for
// job.ArraySequence's own rollbackToStopped when a Start fails at or
// after ConfirmReady (#387, #372):
// hoserva-storage-ready.service is
// RemainAfterExit=yes, so once it has ever succeeded it stays
// "active (exited)" — satisfying hoserva-storage.target's own
// Requires=/After= on it — regardless of what StorageReadyFlagPath says.
// ArraySequence.Stop's own Services loop already stops Samba and NFS
// directly, but neither that nor an unmount touches the gate unit itself,
// so anything that later starts Samba or NFS during maintenance (an
// unattended security update, a hand-run systemctl) still finds
// hoserva-storage.target satisfied and serves the unmounted pool straight
// off the boot disk. persist is the caller's own persist argument: only
// persist=true (a user `array stop`, or rollbackToStopped undoing a
// failed Start) sets the durable array-stopped condition flag
// (disk.StorageStoppedFlagPath, #387) — every
// generated mount unit carries ConditionPathExists=! that flag, which is
// what actually keeps a mount from coming back during maintenance —
// hoserva-storage.target's own Wants=/Requires= is not enough, because
// nfs-utils' own systemd integration derives a RequiresMountsFor=
// directly on nfs-server.service for every NFS-exported path, entirely
// outside any edge Hoserva itself writes, and that reaches the exported
// share's own mount unit — and, through its own RequiresMountsFor=, the
// catch-all and every physical disk under it — regardless of
// hoserva-storage.target's own state. persist=false (StopForShutdown, a
// reboot or a UPS low-battery shutdown) never sets it: neither is a user
// asking the array to stay stopped once the box comes back, and a flag
// set here would fail every mount unit's own ConditionPathExists in the
// boot-time systemd transaction that runs before hoservad is even
// exec'd — long before Startup's own reconciliation from the (untouched)
// persisted row could run. A flag already in force from an earlier
// persisted stop is left exactly as it is either way: this never clears
// it. Close then stops Docker and libvirt — the same enabled/masked-aware
// ServiceUnitController startDependents already uses, mirrored for
// stopping, since a refusal to stop must hold the sequence up exactly
// like a refused Samba/NFS stop, not be skipped past on the way to
// unmounting — then removes the runtime flag before it stops the gate
// unit itself (#387): an smbd or
// NFS start landing between the two steps (an unattended security
// update, a hand-run systemctl) must find the flag already gone, so its
// fixed `test -e` ExecStart fails on its own even before the unit stop
// below reaches it — clearing the flag only after stopping the unit left
// a window where that same start could still find the flag in place and
// pass, immediately before Stop's own unmount served the boot disk
// underneath it.
func (s *storageTargetSync) Close(ctx context.Context, persist bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if persist {
		if err := s.setStoppedFlag(); err != nil {
			return err
		}
	}
	for _, svc := range dependentServicesOutsideArraySequence {
		ctrl := disk.ServiceUnitController{ServiceName: svc, Unit: svc, Runner: s.Runner}
		if err := ctrl.Stop(ctx); err != nil {
			return fmt.Errorf("stopping %s before closing the storage-target gate: %w", svc, err)
		}
	}
	if err := s.clearFlag(); err != nil {
		return err
	}
	readyGate := disk.ServiceUnitController{ServiceName: "storage-ready gate", Unit: pool.StorageReadyUnitName, Runner: s.Runner}
	if err := readyGate.Stop(ctx); err != nil {
		return fmt.Errorf("stopping %s: %w", pool.StorageReadyUnitName, err)
	}
	s.ready, s.applied = false, true
	return nil
}

// Open implements job.StorageTarget for job.ArraySequence.Start (#387):
// removes the array-stopped condition flag Close set, before Start ever
// calls Mount on anything — every generated mount unit carries
// ConditionPathExists=! that flag, so a start would otherwise find it
// still set and silently skip, exactly like the external starts Close's
// own flag exists to block.
func (s *storageTargetSync) Open(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clearStoppedFlag()
}

// Reclose implements job.StorageTarget for job.ArraySequence.Start (#387):
// restores the array-stopped condition flag Open removed, for a Start
// that fails before ConfirmReady ever runs — a disk mount, UR9's own
// disk-identity check, or a catch-all/share mount failure — while
// maintenance mode (and SQLite's persisted row) stays on. Unlike Close, it
// never stops a service and never touches the readiness flag or the gate
// unit: nothing above those mount loops has started a service, a
// dependent, or the gate itself at any point that calls this from there.
// A Start failure at or after ConfirmReady normally never calls this
// instead: ConfirmReady's own failure lands before it ever opens the gate
// or starts Docker/libvirt, but by then every mount call Start's own
// loops made has already succeeded and needs unmounting, which this
// function alone never does. Once ConfirmReady has succeeded, the gate is
// open and Docker/libvirt are running, so a Service Start failure or the
// persisted exited-maintenance write failing happens with that gate open
// (and any already-started Service still running). Either way
// job.ArraySequence's own rollbackToStopped calls Close (persist=true)
// there instead, which both restores this same flag and actually stops
// whatever ConfirmReady or Services actually started, and unmounts
// everything Start had already brought up. rollbackToStopped falls back
// to calling this directly
// whenever its own stopSequence returns an error — a service that fails
// to stop, a failure inside Close itself, or an unmount failure after
// Close has already succeeded — and Close is not the only place that
// restores the flag: this fallback restores it on every such path. Where
// that leaves the gate depends on how far stopSequence got: a service
// that failed to stop before stopSequence ever reached Close, or a
// failure inside Close itself — Close stops Docker then libvirt, clears
// the readiness flag, and only then stops the gate unit, so any failure
// before that last step returns with the gate still open — leaves the
// gate open, with whatever is still running underneath it (Docker,
// libvirt, a VM, a container, Samba or NFS) until an operator clears it
// and retries; only an unmount failure, which runs after Close has
// already returned nil, leaves the gate already closed.
func (s *storageTargetSync) Reclose(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setStoppedFlag()
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
