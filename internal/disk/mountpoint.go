package disk

import (
	"context"
	"errors"
	"fmt"
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
// landing on the boot device." It is a one-time setup step — called once
// when a disk's mountpoint is first assigned (array setup, disk add), not
// on every mount/unmount cycle: the immutable bit lives on the directory
// entry itself, which a later mount transparently shadows (a filesystem
// mount does not require, or clear, its mountpoint's own flags) and a
// later unmount re-exposes unchanged, still immutable. Calling it again
// on an already-immutable, already-empty directory is harmless.
func EnsureEmptyMountpoint(ctx context.Context, r Runner, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("disk: creating mountpoint directory %s: %w", path, err)
	}

	mounted, err := isMountpoint(path)
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
	return nil
}

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
