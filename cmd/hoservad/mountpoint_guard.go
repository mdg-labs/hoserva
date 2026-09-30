package main

import (
	"context"
	"log"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
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

// guardMountpoints makes every array slot's mountpoint immutable while it is
// empty and unmounted (doc 02 §1, Q69), so a write into a slot whose disk did
// not mount fails instead of landing on the boot device. It covers arrays
// that predate the guard and one a bare-metal restore has just described:
// neither ever went through a job that assigned the mountpoint. A slot that
// is already mounted is skipped (disk.GuardMountpoint); it inspects only the
// mountpoint directory itself, never a disk's contents (Q13). A finding on
// one slot — a filesystem that refuses chattr +i, a mountpoint that already
// holds files — is logged and never stops the pass or startup: the slot is
// only left as unprotected as it was before.
func (s *storageTargetSync) guardMountpoints(ctx context.Context, seq *job.ArraySequence) {
	for _, d := range seq.Disks {
		if ctx.Err() != nil {
			return
		}
		if err := s.guardMountpoint(ctx, d.Where()); err != nil {
			log.Printf("hoservad: mountpoint %s is not protected against writes while its disk is unmounted (Q69): %v", d.Where(), err)
		}
	}
}
