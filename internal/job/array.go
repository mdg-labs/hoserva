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
}

// Stop runs doc 02 §4's sequence. It stops on the first error and does
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
	if s.Scheduler != nil {
		if err := s.Scheduler.EnterMaintenance(ctx); err != nil {
			return fmt.Errorf("job: entering maintenance mode: %w", err)
		}
		// EnterMaintenance only signals running jobs to stop and returns;
		// it does not wait for them to actually finish. Proceeding to stop
		// services and unmount storage while one of Hoserva's own jobs
		// (mover, rebalance, evacuation, a Parity-class Sync) is still
		// mid-write is exactly the data-loss scenario this sequence exists
		// to prevent (doc 02 §4) — so Stop waits here for every job that
		// was running to actually exit before touching anything else.
		if err := s.Scheduler.Drain(ctx); err != nil {
			return err
		}
	}

	for _, svc := range s.Services {
		if err := svc.Stop(ctx); err != nil {
			return fmt.Errorf("job: stopping %s: %w", svc.Name(), err)
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
		s.Scheduler.MarkArrayStopped()
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
// the per-share mounts, then every service starts, in the reverse of its
// own Stop order (the last thing stopped is the first thing started).
// Maintenance mode is exited only once every step succeeds. It is refused
// while a data-disk upgrade is pending (doc 02 §4 E6), and once the disks
// are mounted DiskCheck confirms them before anything above them starts
// (UR9): on a mismatch the disks are unmounted again and maintenance mode
// stays on.
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
			s.Scheduler.RestoreArrayStopped(wasStopped)
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
				s.Scheduler.RestoreArrayStopped(wasStopped)
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
	for i := len(s.Services) - 1; i >= 0; i-- {
		svc := s.Services[i]
		if err := svc.Start(ctx); err != nil {
			return fmt.Errorf("job: starting %s: %w", svc.Name(), err)
		}
	}

	if s.Scheduler != nil {
		s.Scheduler.ExitMaintenance()
	}
	return nil
}
