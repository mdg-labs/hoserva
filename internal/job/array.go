package job

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// ArrayService is one system-touching service the doc 02 §4 stop/start
// sequence coordinates around storage: a VM (a graceful-then-forced-after-
// timeout shutdown is that implementation's own responsibility — this
// package calls Stop exactly once and trusts it to embody that policy), a
// container runtime, Samba, NFS. internal/vm, internal/container and
// internal/share each implement this against their own real client once
// they exist; every test in this package uses a fake (CLAUDE.md:
// "every system-touching subsystem sits behind a package interface with a
// scriptable fake").
type ArrayService interface {
	Name() string
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
}

// ArrayMount is one mount the doc 02 §4 sequence tears down or brings up:
// a per-share mergerfs mount, the catch-all, or a physical disk's own
// mount unit. internal/pool's MountController and internal/disk's
// MountUnitController each satisfy this shape without either package
// importing this one.
type ArrayMount interface {
	Where() string
	Mount(ctx context.Context) error
	Unmount(ctx context.Context) error
}

// ReadinessGate is the boot-time readiness check (doc 02 §1, Q69) Start
// consults before mounting anything: disk.StorageGate satisfies this
// shape without this package importing internal/disk.
type ReadinessGate interface {
	// Ready reports whether the array may activate: every expected disk
	// present at the last evaluation, or a human has explicitly
	// acknowledged the degraded state.
	Ready() bool
}

// ErrStorageNotReady is Start's refusal when Gate is set and reports the
// array is not ready to activate (doc 02 §1, Q69) — the array must
// genuinely refuse to activate while degraded and unacknowledged, not
// mount whatever disks happen to be present.
var ErrStorageNotReady = errors.New("job: storage is degraded and unacknowledged — refusing to start the array")

// ErrDiskUpgradeDataPending refuses `array start`, `array stop` and a
// second data-disk upgrade while one is pending (doc 02 §4 E6, E7, E8).
// Callers wrap it with the pending job's id.
var ErrDiskUpgradeDataPending = errors.New("job: a data-disk upgrade is pending")

// ErrArrayDiskMismatch is Start's refusal when a mounted array disk does
// not hold the filesystem SQLite names for it (doc 02 §4 UR9).
var ErrArrayDiskMismatch = errors.New("job: an array disk mountpoint does not hold the disk SQLite names")

// StorageTarget is the boot-ordering gate (doc 02 §1, Q69) Start brings
// current once its own Disks, CatchAll and ShareMounts are all mounted
// and confirmed, and before any Services starts (#372):
// cmd/hoservad's storageTargetSync satisfies this without this package
// importing it. Without this, Start's own Services loop calls systemctl
// start against a unit (Samba, NFS) whose own drop-in binds it to
// hoserva-storage.target while the readiness flag behind that target is
// still whatever an earlier, not-ready boot left it as — every retry
// failing the same way, with the array stuck in maintenance mode.
// Close is the gate's own half of `array stop` (#387):
// hoserva-storage-ready.service is RemainAfterExit=yes, so
// once it has ever succeeded it stays "active (exited)" — satisfying
// hoserva-storage.target's own Requires=/After= on it — regardless of
// what the runtime flag behind it says. Stop's own Services loop above
// already stops Samba and NFS directly, but neither that nor an unmount
// touches the gate unit itself, so anything that later starts Samba or
// NFS during maintenance (an unattended security update, a hand-run
// systemctl) still passes hoserva-storage.target and serves the
// unmounted pool. Close removes the runtime flag and stops the gate unit
// so its fixed `test -e` ExecStart runs — and fails — the next time
// anything needs it, and stops Docker and libvirt (doc 02 §1, Q70): both
// read the pool the way Samba and NFS do, but neither is in Services,
// which has only ever carried Samba and NFS (#309). Close's persist
// argument is stop's own persist (#387): only a persisted user
// `array stop` (Stop) may leave the durable array-stopped condition flag
// (disk.StorageStoppedFlagPath) in force — every generated mount unit
// (every physical disk, the catch-all, every per-share mount) carries
// ConditionPathExists=! that flag, because nfs-utils' own systemd
// integration derives a RequiresMountsFor= directly on nfs-server.service
// for every NFS-exported path, entirely outside any edge Hoserva itself
// writes — confirmed empirically to remount a physical disk when
// `systemctl start nfs-kernel-server` ran during `array stop`, even after
// hoserva-storage.target's own Wants= on the disk mounts was removed,
// because that edge was never involved. StopForShutdown (a reboot or a
// UPS low-battery shutdown) calls Close with persist false: neither is a
// user asking the array to stay stopped once the box comes back, and a
// flag left in force here would make every mount unit's own boot-time
// ConditionPathExists fail — skipped, not merely delayed — in the very
// systemd transaction that runs before hoservad is even exec'd, well
// ahead of Startup's own reconciliation. Open is Start's own reversal of
// a persisted Close, called before it mounts anything: without it,
// hoservad's own first Mount call would find the same condition unmet and
// silently skip too. Reclose is Start's own failure path for a mount or
// identity-check failure that lands before ConfirmReady ever runs — a
// disk mount, UR9's own disk-identity check, or a catch-all/share mount
// failure: once Open has cleared the flag, that failure must put it back
// on its own, since nothing above those mount loops has started a
// service, a dependent, or the gate itself at any point that calls it. A
// failure at or after ConfirmReady instead rolls the whole sequence back
// through ArraySequence's own stop path (stopSequence): by then every
// mount call Start's own loops made has already returned without error —
// though ConfirmReady's own confirmPoolMounted step can still find the
// catch-all not actually a live mount underneath that. ConfirmReady's own
// failure lands before it ever opens the gate or starts Docker/libvirt —
// it can only fail at writing the storage-target units, confirming the
// pool mounted, or setting the readiness flag, all of which run before it
// calls startDependents — so rollbackToStopped there undoes the mounts
// Start's own loops made, never anything ConfirmReady itself started.
// Once ConfirmReady has succeeded, though, the gate is open with
// Docker/libvirt already started, and — once the Services loop below is
// under way — Start may have brought a further service up too, so a
// Service's own Start
// failure or the persisted exited-maintenance write failing needs the
// same rollback: leaving any of that running while reporting Start
// failed would leave the persisted "stopped" row describing a host that
// is nowhere close to true. stopSequence stops every Service Start
// already started, calls this same Close with persist=true (restoring
// this flag, and, if every one of Close's own steps succeeds, also
// stopping Docker/libvirt and closing the gate — never a second
// mechanism), and unmounts the per-share mounts, the catch-all and the
// disks in Stop's own order. stopSequence can fail at any of three
// points, and only the last leaves the gate already closed: a Service
// (Samba or NFS) that refuses to stop before stopSequence ever reaches
// Close leaves the gate open with that service still running; a failure
// inside Close itself — Docker refusing to stop, libvirt refusing to
// stop, the readiness flag failing to clear, or the gate unit itself
// failing to stop — leaves the gate open, since only Close returning
// successfully actually closes it; only an
// unmount failure, which runs after Close has already returned
// successfully, leaves the gate closed already. Whichever of the three
// stops stopSequence, the rollback that called it falls back to calling
// Reclose on its own, restoring the durable flag alone; whatever
// stopSequence didn't reach is left exactly as described above until an
// operator resolves it and retries.
type StorageTarget interface {
	ConfirmReady(ctx context.Context, seq *ArraySequence) error
	Close(ctx context.Context, persist bool) error
	Open(ctx context.Context) error
	Reclose(ctx context.Context) error
}

// ArrayDiskCheck is UR9's check: every array-disk mountpoint that is
// mounted holds the filesystem UUID SQLite names for it.
type ArrayDiskCheck interface {
	ConfirmArrayDisks(ctx context.Context) error
}

// ArrayDiskUUIDCheck confirms each of Disks, when mounted, by filesystem
// UUID from the mount table (doc 02 §4 UR9). Disks come from SQLite.
type ArrayDiskUUIDCheck struct {
	Mounts MountTable
	Disks  []disk.MountUnit
}

// ConfirmArrayDisks returns an ErrArrayDiskMismatch error naming every
// mismatched mountpoint, or the error that kept one from being checked.
func (c ArrayDiskUUIDCheck) ConfirmArrayDisks(ctx context.Context) error {
	var bad []string
	for _, u := range c.Disks {
		mounted, err := c.Mounts.IsMounted(ctx, u.Where)
		if err != nil {
			return fmt.Errorf("job: checking whether %s is mounted: %w", u.Where, err)
		}
		if !mounted {
			continue
		}
		got, err := c.Mounts.MountedUUID(ctx, u.Where)
		if err != nil {
			return fmt.Errorf("job: reading the filesystem mounted at %s: %w", u.Where, err)
		}
		if got != u.UUID {
			bad = append(bad, fmt.Sprintf("%s holds %s, SQLite names %s", u.Where, got, u.UUID))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: %s", ErrArrayDiskMismatch, strings.Join(bad, "; "))
	}
	return nil
}

// PendingUpgradeGate is the storage readiness gate (doc 02 §1, Q69) with
// doc 02 §4 UR2 applied: not ready while a data-disk upgrade is pending,
// or while that cannot be checked.
type PendingUpgradeGate struct {
	Gate      ReadinessGate
	Scheduler *Scheduler
}

// Ready reports Gate.Ready, unless a data-disk upgrade is pending.
func (g PendingUpgradeGate) Ready() bool {
	pending, err := g.Scheduler.PendingDiskUpgradeData(context.Background())
	if err != nil || pending != nil {
		return false
	}
	return g.Gate.Ready()
}

// ArraySequence is doc 02 §4's "Stopping the array" ordering, and Q70's
// own restatement of it: maintenance mode first (new jobs refused,
// resumable jobs stopped at their next checkpoint, the rest marked
// interrupted — Scheduler.EnterMaintenance already does this), then VMs
// shut down, then containers stop, then Samba and NFS stop, and only then
// do the per-share mounts, the catch-all and the disks unmount, in that
// order. Start reverses it. `hoserva array stop|start`, their API
// operations, and system shutdown/reboot (through hoservad's own
// SIGTERM/shutdown handling) all call the same Stop/Start here — one
// implementation, never duplicated per caller.
type ArraySequence struct {
	Scheduler *Scheduler

	// Gate, when set, must report Ready before Start mounts anything
	// (doc 02 §1, Q69). Nil skips the check — every existing caller in
	// this package's own tests predates the gate and has no degraded
	// state to consult.
	Gate ReadinessGate

	// Services is stop order: e.g. VMs, then containers, then Samba, then
	// NFS. Start runs the same slice in reverse.
	Services []ArrayService

	ShareMounts []ArrayMount
	// CatchAll is nil when there is no catch-all mounted yet (a pool that
	// was never brought up in the first place).
	CatchAll ArrayMount
	// Disks are the physical data, parity and cache mounts.
	Disks []ArrayMount
	// DiskCheck, when set, runs once Start has mounted Disks and before
	// anything above them starts (doc 02 §4 UR9).
	DiskCheck ArrayDiskCheck
	// StorageTarget, when set, is called by Start once every mount above
	// has succeeded and before any Services starts (#372). Nil
	// is every existing caller in this package's own tests and the lab,
	// which have no such gate to satisfy.
	StorageTarget StorageTarget
}

// Stop runs doc 02 §4's sequence for a user-requested `array stop`: it
// persists maintenance mode (#387), so a crash or restart while the array
// is stopped this way stays stopped. It stops on the first error and does
// not proceed to unmount anything past that point: unmounting storage a
// service might still be writing to is exactly the data-loss scenario
// this ordering exists to prevent (doc 02 §4 — "a container holding a
// file open blocks the unmount, or a disk is pulled mid-write"), so a
// service that refuses to stop must hold the array up, not be skipped
// past. Maintenance mode is left active on any failure — the array stays
// refusing new jobs until an operator resolves the stuck service and
// retries, rather than silently falling back to normal operation with
// some services already stopped.
func (s ArraySequence) Stop(ctx context.Context) error {
	return s.stop(ctx, true)
}

// StopForShutdown runs the exact same sequence as Stop, for a reboot
// (update.Engine.Reboot) or a UPS low-battery shutdown (UPSShutdown) —
// every job checkpointed or interrupted, every service stopped, the
// storage-target gate closed, everything unmounted — but never persists a
// new "stopped" state (#387): neither is a user asking the
// array to stay stopped once the box comes back, and persisting one here
// would leave RestorePersistedMaintenance holding the array offline after
// the next ordinary boot. A persisted user `array stop` already in force
// is left exactly as it is — this never writes to it, so it can never
// clear one either. For the same reason it never asks StorageTarget.Close
// to leave the durable array-stopped condition flag in force either (#387):
// a flag left behind here would make every mount unit's own
// ConditionPathExists fail in the boot-time systemd transaction that runs
// before hoservad is even exec'd, long before Startup's own reconciliation
// from the (untouched) persisted row could ever run.
func (s ArraySequence) StopForShutdown(ctx context.Context) error {
	return s.stop(ctx, false)
}

func (s ArraySequence) stop(ctx context.Context, persist bool) error {
	if s.Scheduler != nil {
		if persist {
			if err := s.Scheduler.EnterMaintenance(ctx); err != nil {
				return fmt.Errorf("job: entering maintenance mode: %w", err)
			}
		} else {
			s.Scheduler.EnterMaintenanceTransient()
		}
		// EnterMaintenance/EnterMaintenanceTransient only signal running
		// jobs to stop and return; neither waits for them to actually
		// finish. Proceeding to stop services and unmount storage while
		// one of Hoserva's own jobs (mover, rebalance, evacuation, a
		// Parity-class Sync) is still mid-write is exactly the data-loss
		// scenario this sequence exists to prevent (doc 02 §4) — so Stop
		// waits here for every job that was running to actually exit
		// before touching anything else.
		if err := s.Scheduler.Drain(ctx); err != nil {
			return err
		}
		// Share create/update/delete is not a job, so Drain does not
		// wait for it. A mutation admitted before maintenance mode can
		// still be mkdir'ing a branch directory; unmounting first would
		// hide that directory on the root filesystem.
		if err := s.Scheduler.DrainShareMutations(ctx); err != nil {
			return err
		}
	}

	if err := s.stopSequence(ctx, 0, persist); err != nil {
		return err
	}
	if s.Scheduler != nil {
		if persist {
			s.Scheduler.MarkArrayStopped()
		} else {
			s.Scheduler.MarkArrayStoppedTransient()
		}
	}
	return nil
}

// stopSequence runs doc 02 §4's own stop order — every entry of s.Services
// from index from onward, then StorageTarget.Close, then ShareMounts, then
// CatchAll, then Disks — stopping on the first error exactly like a
// user-requested `array stop` does: a service that refuses to stop, or a
// gate that refuses to close, holds the whole sequence up, not skipped
// past on the way to an unmount that could still be serving a write (doc
// 02 §4). stop (Stop/StopForShutdown) calls this with from=0, every
// service in stop order; rollbackToStopped below calls it with from set
// to whichever services a failed Start had already started, so the two
// never diverge into a second stop mechanism.
func (s ArraySequence) stopSequence(ctx context.Context, from int, persist bool) error {
	for i := from; i < len(s.Services); i++ {
		svc := s.Services[i]
		if err := svc.Stop(ctx); err != nil {
			return fmt.Errorf("job: stopping %s: %w", svc.Name(), err)
		}
	}
	// Closing the storage-target gate (#387) runs here, with Services
	// already stopped and before any unmount: like Services, a service it
	// stops (Docker, libvirt) refusing to release the pool must hold the
	// whole sequence up, not be skipped past on the way to unmounting.
	// persist is threaded straight through: only a genuinely stopped array
	// (Stop itself, or rollbackToStopped undoing a failed Start) may leave
	// the durable array-stopped condition flag in force — StopForShutdown's
	// transient stop must never create it.
	if s.StorageTarget != nil {
		if err := s.StorageTarget.Close(ctx, persist); err != nil {
			return fmt.Errorf("job: closing the storage-target gate: %w", err)
		}
	}
	for _, m := range s.ShareMounts {
		if err := m.Unmount(ctx); err != nil {
			return fmt.Errorf("job: unmounting %s: %w", m.Where(), err)
		}
	}
	if s.CatchAll != nil {
		if err := s.CatchAll.Unmount(ctx); err != nil {
			return fmt.Errorf("job: unmounting %s: %w", s.CatchAll.Where(), err)
		}
	}
	for _, d := range s.Disks {
		if err := d.Unmount(ctx); err != nil {
			return fmt.Errorf("job: unmounting %s: %w", d.Where(), err)
		}
	}
	return nil
}

// RefreshLive applies this sequence's catch-all and share mounts to a
// pool that is already running — a disk added to a live array joins every
// branch list at once (doc 02 §4 "Adding a disk" step 6) instead of at
// the next array start. Each Mount updates an already-live mount in place
// (pool.Mounter) and mounts one that is missing. A stopped array is left
// alone; Start mounts it with the same mounts.
func (s ArraySequence) RefreshLive(ctx context.Context, running bool) error {
	if !running || s.CatchAll == nil {
		return nil
	}
	if err := s.CatchAll.Mount(ctx); err != nil {
		return fmt.Errorf("job: updating %s: %w", s.CatchAll.Where(), err)
	}
	for _, m := range s.ShareMounts {
		if err := m.Mount(ctx); err != nil {
			return fmt.Errorf("job: updating %s: %w", m.Where(), err)
		}
	}
	return nil
}

// recloseAfterFailedStart restores the array-stopped condition flag Open
// removed, for a Start that fails before ConfirmReady ever runs — a disk
// mount, UR9's own disk-identity check, or a catch-all/share mount
// failure: without this, the flag would be missing while maintenance mode
// (and SQLite's persisted row) stays on, so a boot-time cascade like
// nfs-utils' own RequiresMountsFor= could still remount storage mid-swap
// even though `array start` never actually finished. Nothing above those
// mount loops has started a service, a dependent, or the storage-target
// gate at any point this runs, so restoring the flag alone is enough — a
// failure at or after ConfirmReady is rollbackToStopped's own job below,
// never this function's. This is a compensating action running after a
// failure the caller already reported, not the operation the caller's own
// context was issued for, so it runs on context.WithoutCancel(ctx) (#387):
// Start is reached from an HTTP request (internal/api's StartArray passes
// its own request context straight through), and a client that disconnects
// the moment its own mount failure lands must never also take this restore
// down with it — leaving the flag missing while SQLite still says stopped.
func (s ArraySequence) recloseAfterFailedStart(ctx context.Context) error {
	if s.StorageTarget == nil {
		return nil
	}
	if err := s.StorageTarget.Reclose(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("job: restoring the array-stopped condition flag: %w", err)
	}
	return nil
}

// rollbackToStopped reverses a Start that failed at or after ConfirmReady:
// by then every mount call Start's own loops made has already returned
// without error — though ConfirmReady's own confirmPoolMounted step can
// still find the catch-all not actually a live mount underneath that. A
// ConfirmReady failure itself lands before ConfirmReady ever opens the
// storage-target gate or starts Docker/libvirt — writeUnits,
// confirmPoolMounted and setFlagReady, its own three steps, all run before it calls
// startDependents, so on that failure the gate was never opened, Docker
// and libvirt never started, and the pool may not even be mounted yet.
// Once ConfirmReady has succeeded, though, the gate is open with
// Docker/libvirt started, and — once the Services loop is under way —
// Start may have brought a VM, a container, Samba or NFS up too, so a
// Service's own Start failure or the persisted exited-maintenance write
// failing still needs this same rollback. Leaving any of that running
// while Start still returns an error would leave the persisted row this failure
// leaves behind (maintenance stays on; ExitMaintenanceChecked never ran or
// never took effect) describing a host that is nowhere close to true: a
// user who trusts a "stopped" reading could pull a disk from a pool that,
// underneath, is still fully live. from is the lowest index in s.Services
// the failed Start had already started — startedIndex+1 for a Services-
// loop failure (Start's own loop counts down, so everything above the
// failed index already succeeded), 0 for an ExitMaintenanceChecked failure
// (every entry already started), or len(s.Services) for a ConfirmReady
// failure itself (the Services loop never ran). stopSequence is the exact
// stop path Stop/StopForShutdown use — never a second mechanism — so this
// always persists the durable array-stopped flag (persist=true, the same
// as a genuine `array stop`) through the same StorageTarget.Close call.
//
// This whole rollback is itself a compensating action, not the operation
// the caller's context was issued for (#387): Start is reached from an
// HTTP request (internal/api's StartArray passes its own request context
// straight through to Start unchanged), so a client disconnecting the
// instant a Service's own Start fails would otherwise cancel every
// exec.CommandContext call this rollback makes too — including the very
// first Service.Stop in stopSequence, which runs before StorageTarget
// .Close ever does. A rollback cut short that way — a Service.Stop
// cancelled before it ever ran, before Close is even reached — would
// otherwise leave Docker, libvirt and every remaining service running and
// the pool still mounted, while the caller that triggered it is already
// gone and never sees the error. The Reclose fallback below still
// restores the durable flag on that same path regardless, since it runs
// unconditionally whenever stopSequence returns any error: what
// context.WithoutCancel(ctx), applied here before stopSequence or that
// fallback ever run, actually buys is the rest of the rollback — stopping
// the remaining services, closing the gate, unmounting — running to
// completion regardless of what happens to the request that started it,
// rather than being cut short by that same cancellation.
//
// Close is not the only place that restores the durable flag: whenever
// stopSequence returns any error — a service that refuses to stop before
// ever reaching Close, a failure inside Close itself, or an unmount
// failure after Close has already succeeded — Reclose restores the same
// durable flag on its own. The persisted row already says the array is
// stopped (or is about to), and the flag it depends on must never
// disagree with it, even though the rest of the rollback that stopSequence
// didn't reach — stopping the remaining services, closing the gate,
// unmounting — still needs an operator to resolve whatever stopSequence
// actually failed on (a service that refused to stop, Docker or libvirt
// refusing to stop inside Close, or a failed unmount) and retry — the same
// way a plain `array stop` leaves maintenance mode on and asks for a retry
// rather than silently declaring success (doc 02 §4).
func (s ArraySequence) rollbackToStopped(ctx context.Context, from int) error {
	ctx = context.WithoutCancel(ctx)
	err := s.stopSequence(ctx, from, true)
	if err == nil {
		return nil
	}
	if s.StorageTarget != nil {
		if rerr := s.StorageTarget.Reclose(ctx); rerr != nil {
			return fmt.Errorf("job: could not return the array to stopped: %w (restoring the array-stopped flag also failed: %v)", err, rerr)
		}
	}
	return fmt.Errorf("job: could not return the array to stopped: %w", err)
}

// Start reverses Stop: the disks mount first, then the catch-all, then
// the per-share mounts, then StorageTarget (if set) confirms the boot-
// ordering gate is current, then every service starts, in the reverse of
// its own Stop order (the last thing stopped is the first thing
// started). Maintenance mode is exited only once every step succeeds. It
// is refused while a data-disk upgrade is pending (doc 02 §4 E6), and
// once the disks are mounted DiskCheck confirms them before anything
// above them starts (UR9): on a mismatch the disks are unmounted again
// and maintenance mode stays on. StorageTarget runs before Services for
// the same reason: Samba and NFS's own drop-ins bind them to
// hoserva-storage.target, so starting them against a gate this boot's
// own evaluation left closed would fail every time (#372). A
// failure at or after that point — ConfirmReady itself, a Service's own
// Start, or the persisted exited-maintenance write — never just restores
// the durable flag: it rolls the whole sequence back through
// rollbackToStopped, so a service or dependent StorageTarget already
// brought up is actually stopped again before Start returns, not left
// running underneath a persisted row that still says stopped.
func (s ArraySequence) Start(ctx context.Context) error {
	// A start that fails before leaving anything mounted puts the
	// "stop completed" state back as it was, so a data-disk upgrade stays
	// admissible without another `array stop`.
	var wasStopped bool
	if s.Scheduler != nil {
		var err error
		if wasStopped, err = s.Scheduler.BeginArrayStart(ctx); err != nil {
			return err
		}
	}
	if s.Gate != nil && !s.Gate.Ready() {
		if s.Scheduler != nil {
			if rerr := s.Scheduler.RestoreArrayStopped(wasStopped); rerr != nil {
				return errors.Join(ErrStorageNotReady, rerr)
			}
		}
		return ErrStorageNotReady
	}
	// Open removes the array-stopped condition flag (#387) before any
	// mount below runs — every one of them now carries
	// ConditionPathExists=!disk.StorageStoppedFlagPath, so this must clear
	// before hoservad's own first Mount call or every one of them would
	// silently skip.
	if s.StorageTarget != nil {
		if err := s.StorageTarget.Open(ctx); err != nil {
			if s.Scheduler != nil {
				if rerr := s.Scheduler.RestoreArrayStopped(wasStopped); rerr != nil {
					return errors.Join(fmt.Errorf("job: opening the storage target: %w", err), rerr)
				}
			}
			return fmt.Errorf("job: opening the storage target: %w", err)
		}
	}

	for _, d := range s.Disks {
		if err := d.Mount(ctx); err != nil {
			return errors.Join(fmt.Errorf("job: mounting %s: %w", d.Where(), err), s.recloseAfterFailedStart(ctx))
		}
	}
	if s.DiskCheck != nil {
		if err := s.DiskCheck.ConfirmArrayDisks(ctx); err != nil {
			var errs []error
			for _, d := range s.Disks {
				if uerr := d.Unmount(ctx); uerr != nil {
					errs = append(errs, fmt.Errorf("unmounting %s again: %w", d.Where(), uerr))
				}
			}
			// Every disk unmounted again: the array is back where it was.
			// Any failed unmount leaves it not stopped.
			if len(errs) == 0 && s.Scheduler != nil {
				if rerr := s.Scheduler.RestoreArrayStopped(wasStopped); rerr != nil {
					errs = append(errs, rerr)
				}
			}
			if rerr := s.recloseAfterFailedStart(ctx); rerr != nil {
				errs = append(errs, rerr)
			}
			return errors.Join(append([]error{err}, errs...)...)
		}
	}
	if s.CatchAll != nil {
		if err := s.CatchAll.Mount(ctx); err != nil {
			return errors.Join(fmt.Errorf("job: mounting %s: %w", s.CatchAll.Where(), err), s.recloseAfterFailedStart(ctx))
		}
	}
	for _, m := range s.ShareMounts {
		if err := m.Mount(ctx); err != nil {
			return errors.Join(fmt.Errorf("job: mounting %s: %w", m.Where(), err), s.recloseAfterFailedStart(ctx))
		}
	}
	if s.StorageTarget != nil {
		// A ConfirmReady failure lands before it ever opens the
		// storage-target gate or starts Docker/libvirt — writeUnits,
		// confirmPoolMounted and setFlagReady, ConfirmReady's own three
		// steps, all run before it calls startDependents — so
		// rollbackToStopped, not a plain Reclose, undoes the mounts Start's
		// own loops already brought up, not anything ConfirmReady itself
		// started. No entry in s.Services has started yet (the loop below
		// has not run), so from is every one of them.
		if err := s.StorageTarget.ConfirmReady(ctx, &s); err != nil {
			return errors.Join(fmt.Errorf("job: confirming storage-target readiness: %w", err), s.rollbackToStopped(ctx, len(s.Services)))
		}
	}
	for i := len(s.Services) - 1; i >= 0; i-- {
		svc := s.Services[i]
		if err := svc.Start(ctx); err != nil {
			// s.Services[i+1:] already started (this loop counts down), so
			// the rollback stops exactly those, then closes the gate and
			// unmounts everything ConfirmReady above already confirmed live.
			return errors.Join(fmt.Errorf("job: starting %s: %w", svc.Name(), err), s.rollbackToStopped(ctx, i+1))
		}
	}

	if s.Scheduler != nil {
		// A failed persisted write here must fail Start itself: every mount
		// and every service above has already succeeded, so leaving SQLite
		// still holding the previous, stopped state while reporting this
		// call as successful would let a restart before the user retries
		// put the daemon straight back into maintenance mode over an array
		// that is actually live. That failure also leaves maintenance still
		// on (ExitMaintenanceChecked never flips it in memory when the
		// write itself fails) with every service and dependent already up,
		// so rollbackToStopped — not a plain Reclose — actually stops all of
		// s.Services (every entry already started) as well as restoring the
		// durable flag.
		if err := s.Scheduler.ExitMaintenanceChecked(); err != nil {
			return errors.Join(fmt.Errorf("job: exiting maintenance mode: %w", err), s.rollbackToStopped(ctx, 0))
		}
	}
	return nil
}

// RegenerateArrayMountsFromStore rewrites every managed disk and pool
// mount unit — every physical data/parity/cache mount, snapraid.conf, the
// catch-all, and every per-share and mover-target pool mount — from
// SQLite (D4), without mounting or unmounting anything (#387).
// cmd/hoservad's Startup calls this before evaluating readiness: an array
// or a share created before the ConditionPathExists=!
// disk.StorageStoppedFlagPath line existed in disk.MountUnit.Render and
// pool.Mount.Render left its own unit condition-less forever, since the
// only other writers of those files are array creation, a share mutation,
// and the disk add/replace/upgrade flows — none of which a plain restart
// or a package upgrade on an otherwise untouched array ever runs again.
// Left condition-less, that unit stays reachable through the boot-time
// cascade — hoserva-storage.target's own soft ordering, and nfs-utils' own
// RequiresMountsFor= on nfs-server.service — straight through `array
// stop`.
func RegenerateArrayMountsFromStore(ctx context.Context, arrays *store.ArrayStore, shares *store.ShareStore, g *config.Generator, now time.Time) error {
	settings, disks, err := arrays.GetArray(ctx)
	if err != nil {
		return err
	}
	units, err := mountUnitsFromStore(disks)
	if err != nil {
		return err
	}
	if err := g.WriteDiskMounts(ctx, units, arrayCreateCommand, 1, now); err != nil {
		return err
	}
	body, err := layoutFromStore(disks).Render()
	if err != nil {
		return err
	}
	if err := g.Write(ctx, config.File{
		Path:    "snapraid.conf",
		Command: arrayCreateCommand,
		Body:    []byte(body),
	}, 1, now); err != nil {
		return err
	}

	state := poolStateFromStore(settings, disks)
	shareRows, err := shares.List(ctx)
	if err != nil {
		return fmt.Errorf("job: listing shares to regenerate pool mounts: %w", err)
	}
	for _, sh := range shareRows {
		state.Shares = append(state.Shares, config.PoolShare{
			Name:         sh.Name,
			CacheMode:    pool.CacheMode(sh.CacheMode),
			CreatePolicy: pool.CreatePolicy(sh.CreatePolicy),
		})
	}
	return g.WritePoolMounts(ctx, state, arrayCreateCommand, 1, now)
}
