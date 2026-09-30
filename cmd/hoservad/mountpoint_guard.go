package main

import (
	"context"
	"log"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
)

// newArrayDiskMounter is the UnitMounter every job that assigns an array
// slot's mountpoint — array create, disk add, disk replace, a parity
// upgrade — mounts through: inner, with the slot's mountpoint made
// immutable while empty before its disk is first mounted there (doc 02 §1,
// Q69).
func newArrayDiskMounter(runner disk.Runner, inner disk.UnitMounter) disk.UnitMounter {
	return disk.GuardedMounter{Mounter: inner, Runner: runner}
}

func (s *storageTargetSync) guardMountpoint(ctx context.Context, path string) error {
	if s.MountpointGuard != nil {
		return s.MountpointGuard(ctx, path)
	}
	return disk.GuardMountpoint(ctx, s.Runner, path)
}

// guardMountpoints makes every array slot's mountpoint, and the pool's
// catch-all mountpoint, immutable while it is empty and unmounted (doc 02 §1,
// Q69), so a write into a slot whose disk did not mount, or under /mnt/user
// while the pool is down, fails instead of landing on the boot device. It
// covers arrays that predate the guard and one a bare-metal restore has just
// described: neither ever went through a job that assigned the mountpoint.
// The per-share mountpoints live inside the catch-all mount and need none. A
// path that is already mounted is skipped (disk.GuardMountpoint); the pass
// inspects only the mountpoint directory itself, never a disk's contents
// (Q13). A finding on one path — a filesystem that refuses chattr +i, a
// mountpoint that already holds files — is logged and never stops the pass or
// startup: the path is only left as unprotected as it was before.
func (s *storageTargetSync) guardMountpoints(ctx context.Context, seq *job.ArraySequence) {
	wheres := make([]string, 0, len(seq.Disks)+1)
	for _, d := range seq.Disks {
		wheres = append(wheres, d.Where())
	}
	if seq.CatchAll != nil {
		wheres = append(wheres, seq.CatchAll.Where())
	}
	for _, where := range wheres {
		if ctx.Err() != nil {
			return
		}
		if err := s.guardMountpoint(ctx, where); err != nil {
			log.Printf("hoservad: mountpoint %s is not protected against writes while it is unmounted (Q69): %v", where, err)
		}
	}
}

// arrayPathGuard is disk.GuardMountpoint, replaced only by tests that must
// not create directories under /mnt.
var arrayPathGuard = disk.GuardMountpoint

// guardArrayPath makes where immutable while empty on the array stop/start
// path (doc 02 §1, Q69). A mounted path is skipped (disk.GuardMountpoint); a
// finding is logged and never fails the mount or the unmount.
func guardArrayPath(ctx context.Context, runner disk.Runner, where string) {
	if err := arrayPathGuard(ctx, runner, where); err != nil {
		log.Printf("hoservad: mountpoint %s is not protected against writes while it is unmounted (Q69): %v", where, err)
	}
}

// guardedCatchAllMounter wraps the LiveMounter of the pool's catch-all mount
// so /mnt/user is made immutable while empty (doc 02 §1, Q69) on the array
// stop/start path itself, not only on the Startup and Update passes: it is
// guarded right after Unmount brings the pool down — the moment a write from
// outside Hoserva's gated services would start reaching the boot device,
// including on an install whose pool stayed mounted across every daemon
// restart — and again just before Mount brings it back.
type guardedCatchAllMounter struct {
	inner  pool.LiveMounter
	runner disk.Runner
}

func (g guardedCatchAllMounter) Mount(ctx context.Context, mnt pool.Mount) error {
	guardArrayPath(ctx, g.runner, mnt.Where)
	return g.inner.Mount(ctx, mnt)
}

func (g guardedCatchAllMounter) Unmount(ctx context.Context, where string) error {
	if err := g.inner.Unmount(ctx, where); err != nil {
		return err
	}
	guardArrayPath(ctx, g.runner, where)
	return nil
}

// guardedSlotMount wraps an array slot's mount unit controller so the slot's
// mountpoint is guarded on the array stop/start path the same way the
// catch-all's is: right after Unmount brings the disk down, and again just
// before Mount brings it back. The stop-time guard is what protects a slot
// that was mounted at every Startup and Update pass (those skip a mounted
// path, so an install upgraded in place without a reboot never guarded it):
// a host job writing to /mnt/diskN while the array is stopped hits an
// immutable directory instead of the boot device.
type guardedSlotMount struct {
	inner  job.ArrayMount
	runner disk.Runner
}

func (g guardedSlotMount) Where() string { return g.inner.Where() }

func (g guardedSlotMount) Mount(ctx context.Context) error {
	guardArrayPath(ctx, g.runner, g.inner.Where())
	return g.inner.Mount(ctx)
}

func (g guardedSlotMount) Unmount(ctx context.Context) error {
	if err := g.inner.Unmount(ctx); err != nil {
		return err
	}
	guardArrayPath(ctx, g.runner, g.inner.Where())
	return nil
}
