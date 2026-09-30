package disk

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

// ErrMountpointMounted and ErrMountpointNotEmpty are EnsureEmptyMountpoint's
// refusals (doc 02 §1, Q69): setting the immutable bit is only meaningful
// on an empty, unmounted directory. A path that is already mounted would
// have the bit applied to the directory entry the mount is currently
// hiding, not to anything a stray write could reach; a non-empty path was
// never the freshly assigned, still-bare mountpoint EnsureEmptyMountpoint
// exists for, so chattr +i is refused on both rather than silently landing
// the immutable bit somewhere unintended.
var (
	ErrMountpointMounted  = errors.New("disk: mountpoint is already mounted")
	ErrMountpointNotEmpty = errors.New("disk: mountpoint is not empty")
)

// EnsureEmptyMountpoint creates path (and any missing parent) and marks it
// immutable (doc 02 §1, Q69): "every mountpoint directory is made
// immutable while empty, so a write to an unmounted path fails instead of
// landing on the boot device." It is called when a disk's mountpoint is
// first assigned (array setup, disk add, replace) and by the daemon's
// idempotent pass over every slot, not on every mount/unmount cycle: the
// immutable bit lives on the directory entry itself, which a later mount
// transparently shadows (a filesystem mount does not require, or clear,
// its mountpoint's own flags) and a later unmount re-exposes unchanged,
// still immutable. Calling it again on an already-immutable,
// already-empty directory is harmless.
func EnsureEmptyMountpoint(ctx context.Context, r Runner, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("disk: creating mountpoint directory %s: %w", path, err)
	}

	mounted, err := mountCheck(path)
	if err != nil {
		return fmt.Errorf("disk: checking whether %s is already mounted: %w", path, err)
	}
	if mounted {
		return fmt.Errorf("%w: %s", ErrMountpointMounted, path)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("disk: reading mountpoint directory %s: %w", path, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s", ErrMountpointNotEmpty, path)
	}

	if err := SetImmutable(ctx, r, path, true); err != nil {
		return err
	}

	// The disk's own nofail mount unit can activate between the check above
	// and the chattr, which then set the flag on the mounted filesystem's
	// root, not on the directory it shadows: undo it there, so the disk's
	// top level stays writable.
	mounted, err = mountCheck(path)
	if err != nil {
		return fmt.Errorf("disk: rechecking whether %s was mounted while it was being marked immutable: %w", path, err)
	}
	if mounted {
		if err := SetImmutable(ctx, r, path, false); err != nil {
			return fmt.Errorf("disk: %s was mounted while it was being marked immutable, and clearing the flag from the mounted filesystem's root failed: %w", path, err)
		}
		return fmt.Errorf("%w: %s", ErrMountpointMounted, path)
	}
	return nil
}

// mountCheck is isMountpoint; tests replace it to make a mount appear
// between EnsureEmptyMountpoint's two checks.
var mountCheck = isMountpoint

// isMountpoint reports whether path is currently a mount point: its
// device ID differs from its parent directory's, the same test the
// `mountpoint(1)` tool itself uses. It does no exec and touches nothing —
// a stat of path and its parent — so it is safe to call on the host as
// well as in the lab.
func isMountpoint(path string) (bool, error) {
	dev, err := deviceID(path)
	if err != nil {
		return false, err
	}
	parentDev, err := deviceID(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	return dev != parentDev, nil
}

func deviceID(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("disk: cannot determine device id for %s", path)
	}
	return uint64(st.Dev), nil
}

// SetImmutable sets or clears the filesystem immutable attribute on path
// via chattr — the mechanism doc 02 §1 relies on to make a write into an
// unmounted mountpoint fail (EPERM) rather than silently land on the boot
// device. Only root (CAP_LINUX_IMMUTABLE) can set or clear it; production
// hoservad always runs as root, but a capability-narrowed environment
// (the loop-device lab, doc 06 §3) may refuse it, which this returns as
// an ordinary error rather than panicking.
func SetImmutable(ctx context.Context, r Runner, path string, immutable bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	flag := "+i"
	if !immutable {
		flag = "-i"
	}
	if _, err := r.Run(ctx, "chattr", flag, path); err != nil {
		return fmt.Errorf("disk: chattr %s %s: %w", flag, path, err)
	}
	return nil
}

// GuardMountpoint is EnsureEmptyMountpoint for a caller that may meet a
// slot whose disk is already mounted — an idempotent pass over every slot
// (startup, a topology rebuild) or a mount that is about to be skipped
// anyway: a mounted path is left alone and reported as success, since the
// filesystem serving it needs no guard and chattr +i there would land on
// the mounted filesystem's own root, not on the directory a stray write
// would reach. Every other outcome — a non-empty directory
// (ErrMountpointNotEmpty), a chattr that failed — is returned, never
// swallowed: the mountpoint is then unguarded and the caller must report
// it (Q69).
func GuardMountpoint(ctx context.Context, r Runner, path string) error {
	err := EnsureEmptyMountpoint(ctx, r, path)
	if errors.Is(err, ErrMountpointMounted) {
		return nil
	}
	return err
}

// GuardedMounter wraps the UnitMounter that brings an array slot's disk
// up (data, parity, cache) so the slot's mountpoint is made immutable
// while empty (doc 02 §1, Q69) before its disk is first mounted there:
// array creation, disk add, disk replace and a parity upgrade all mount
// a newly assigned slot through their UnitMounter, so wrapping that one
// mounter guards each of them. A guard finding — a non-empty mountpoint,
// a filesystem that refuses chattr +i — is logged and never fails the
// mount: the disk still has to come up, and the slot is only left as
// unprotected as it was before this guard existed. Unmount is passed
// through unchanged.
type GuardedMounter struct {
	Mounter UnitMounter
	Runner  Runner
	// Ensure overrides GuardMountpoint — set only by tests that must not
	// create directories at a production path.
	Ensure func(ctx context.Context, r Runner, path string) error
}

func (m GuardedMounter) Mount(ctx context.Context, unit MountUnit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ensure := m.Ensure
	if ensure == nil {
		ensure = GuardMountpoint
	}
	if err := ensure(ctx, m.Runner, unit.Where); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		log.Printf("disk: mountpoint %s is not protected against writes while its disk is unmounted (Q69): %v", unit.Where, err)
	}
	return m.Mounter.Mount(ctx, unit)
}

func (m GuardedMounter) Unmount(ctx context.Context, unit MountUnit) error {
	return m.Mounter.Unmount(ctx, unit)
}

var _ UnitMounter = GuardedMounter{}
