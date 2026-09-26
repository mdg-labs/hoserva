package job

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mdg-labs/hoserva/internal/disk"
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
// and confirmed, and before any Services starts (#372 finding 1):
// cmd/hoservad's storageTargetSync satisfies this without this package
// importing it. Without this, Start's own Services loop calls systemctl
// start against a unit (Samba, NFS) whose own drop-in binds it to
// hoserva-storage.target while the readiness flag behind that target is
// still whatever an earlier, not-ready boot left it as — every retry
// failing the same way, with the array stuck in maintenance mode.
// Close is the gate's own half of `array stop` (#387, raised by the #372
// verifier): hoserva-storage-ready.service is RemainAfterExit=yes, so
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
// which has only ever carried Samba and NFS (#309).
type StorageTarget interface {
	ConfirmReady(ctx context.Context, seq *ArraySequence) error
	Close(ctx context.Context) error
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
	// has succeeded and before any Services starts (#372 finding 1). Nil
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
// new "stopped" state (#387 finding 2): neither is a user asking the
// array to stay stopped once the box comes back, and persisting one here
// would leave RestorePersistedMaintenance holding the array offline after
// the next ordinary boot. A persisted user `array stop` already in force
// is left exactly as it is — this never writes to it, so it can never
// clear one either.
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

	for _, svc := range s.Services {
		if err := svc.Stop(ctx); err != nil {
			return fmt.Errorf("job: stopping %s: %w", svc.Name(), err)
		}
	}
	// Closing the storage-target gate (#387) runs here, with Services
	// already stopped and before any unmount: like Services, a service it
	// stops (Docker, libvirt) refusing to release the pool must hold the
	// whole sequence up, not be skipped past on the way to unmounting.
	if s.StorageTarget != nil {
		if err := s.StorageTarget.Close(ctx); err != nil {
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
	if s.Scheduler != nil {
		if persist {
			s.Scheduler.MarkArrayStopped()
		} else {
			s.Scheduler.MarkArrayStoppedTransient()
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
// own evaluation left closed would fail every time (#372 finding 1).
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

	for _, d := range s.Disks {
		if err := d.Mount(ctx); err != nil {
			return fmt.Errorf("job: mounting %s: %w", d.Where(), err)
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
			return errors.Join(append([]error{err}, errs...)...)
		}
	}
	if s.CatchAll != nil {
		if err := s.CatchAll.Mount(ctx); err != nil {
			return fmt.Errorf("job: mounting %s: %w", s.CatchAll.Where(), err)
		}
	}
	for _, m := range s.ShareMounts {
		if err := m.Mount(ctx); err != nil {
			return fmt.Errorf("job: mounting %s: %w", m.Where(), err)
		}
	}
	if s.StorageTarget != nil {
		if err := s.StorageTarget.ConfirmReady(ctx, &s); err != nil {
			return fmt.Errorf("job: confirming storage-target readiness: %w", err)
		}
	}
	for i := len(s.Services) - 1; i >= 0; i-- {
		svc := s.Services[i]
		if err := svc.Start(ctx); err != nil {
			return fmt.Errorf("job: starting %s: %w", svc.Name(), err)
		}
	}

	if s.Scheduler != nil {
		// A failed persisted write here (#387 finding 3) must fail Start
		// itself: every mount and every service above has already
		// succeeded, so leaving SQLite still holding the previous,
		// stopped state while reporting this call as successful would let
		// a restart before the user retries put the daemon straight back
		// into maintenance mode over an array that is actually live.
		if err := s.Scheduler.ExitMaintenanceChecked(); err != nil {
			return fmt.Errorf("job: exiting maintenance mode: %w", err)
		}
	}
	return nil
}
