package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// A source disk is only ever read: it is mounted read-only through
// disk.ReadOnlyMounter (xfs with norecovery, ext4 with noload, btrfs with rescue=nologreplay), at a
// private mountpoint under the session's directory, and released before the
// scan ends. The array's own mount units, which are read-write, are never used.
const (
	// mountsDir is the directory under Service.Dir that holds the private
	// mountpoints, one per mount that can be live at a time.
	mountsDir = "mnt"
	// dataMount and dirsMount are the mountpoints a scan's data-disk read and the
	// DiskReader use. A scan runs one at a time, and the two never overlap.
	dataMount = "data"
	dirsMount = "dirs"

	diskUnmountTimeout = 30 * time.Second
)

// ErrDiskMount is wrapped by a failure to mount a source disk read-only.
var ErrDiskMount = errors.New("the disk could not be mounted read-only")

// readableFS maps the filesystem type udev reports to the one a source disk is
// mounted as. Only these are adopted (Q23).
func readableFS(udev string) (disk.FilesystemType, bool) {
	switch udev {
	case "xfs":
		return disk.XFS, true
	case "ext4":
		return disk.EXT4, true
	case "btrfs":
		return disk.BTRFS, true
	}
	return "", false
}

// withDisk mounts the filesystem of d read-only at where, runs fn on its root and
// unmounts again, whatever fn returned. A mount a killed process left at where
// is released first, and the mount is refused when it cannot be. The error of an
// unmount that did not release the mount is joined to fn's: the result of a
// read that left its disk mounted is not used. The unmount is made without the
// scan's cancellation, so a cancelled scan still releases its mount.
func withDisk(ctx context.Context, m disk.ReadOnlyMounter, d disk.Disk, fsType disk.FilesystemType, where string, fn func(root string) error) (err error) {
	if m == nil {
		return fmt.Errorf("%w: this daemon has no read-only mounter", ErrDiskMount)
	}
	if !strings.HasPrefix(d.FSDevice, "/dev/") || d.FSUUID == "" {
		return fmt.Errorf("%w: udev reports no filesystem node and UUID for %s", ErrDiskMount, d.Device)
	}
	if err := os.MkdirAll(filepath.Dir(where), 0o700); err != nil {
		return fmt.Errorf("%w: %w", ErrDiskMount, err)
	}
	if mounted, err := m.IsMounted(ctx, where); err != nil {
		return fmt.Errorf("%w: %w", ErrDiskMount, err)
	} else if mounted {
		if err := m.Unmount(ctx, where); err != nil {
			return fmt.Errorf("%w: a previous mount is still at %s: %w", ErrDiskRelease, where, err)
		}
	}
	if err := m.MountReadOnly(ctx, string(fsType), d.FSDevice, d.FSUUID, where); err != nil {
		return fmt.Errorf("%w: %w", ErrDiskMount, err)
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), diskUnmountTimeout)
		defer cancel()
		if uerr := m.Unmount(uctx, where); uerr != nil {
			err = errors.Join(err, fmt.Errorf("%w: unmounting %s: %w", ErrDiskRelease, where, uerr))
		}
	}()
	return fn(where)
}

// releaseMounts unmounts every private mountpoint under dir that is mounted:
// the mounts a killed process left. It returns the errors of the mounts it could
// not release.
func releaseMounts(ctx context.Context, m disk.ReadOnlyMounter, dir string) error {
	entries, err := os.ReadDir(filepath.Join(dir, mountsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var errs []error
	for _, n := range names {
		where := filepath.Join(dir, mountsDir, n)
		mounted, err := m.IsMounted(ctx, where)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if mounted {
			if err := m.Unmount(ctx, where); err != nil {
				errs = append(errs, fmt.Errorf("a previous mount is still at %s: %w", where, err))
			}
		}
	}
	return errors.Join(errs...)
}

// DiskReader is the DiskDirs of a daemon that can read the disks: it mounts a
// matched disk read-only, lists what is at its top level, and unmounts. It
// reads data disks and pool devices alike, and refuses a device it cannot show
// is an ordinary single filesystem of a type Hoserva adopts. A device whose
// log holds changes a no-replay mount would not show (ext4 needs_recovery,
// btrfs log_root) is not read: its view would have a hole in it.
type DiskReader struct {
	Mounter disk.ReadOnlyMounter
	// Runner runs the read-only inspection of a device's log before it is
	// mounted. Without one no ext4 or btrfs device is read.
	Runner disk.Runner
	// Dir is the session's directory; the mountpoint is under it.
	Dir string
}

var (
	_ DiskDirs = (*DiskReader)(nil)
	_ DirSizer = (*DiskReader)(nil)
)

// DirSizer measures one top-level directory of a disk, for a disk the scan does
// not read in full (a cache pool device).
type DirSizer interface {
	DirBytes(ctx context.Context, d disk.Disk, name string) (int64, error)
}

func (r *DiskReader) mount(ctx context.Context, d disk.Disk, fn func(root string) error) error {
	if d.UnraidBoot || disk.IsUnraidStick(d) {
		return fmt.Errorf("%s is an Unraid boot device, which is never read as a data area", d.Device)
	}
	fsType, ok := readableFS(d.Filesystem)
	if !ok {
		return fmt.Errorf("%s holds %q, which is not a filesystem Hoserva reads", d.Device, d.Filesystem)
	}
	if fsType == disk.EXT4 || fsType == disk.BTRFS {
		if r.Runner == nil {
			return fmt.Errorf("%s: this daemon cannot check whether its %s log is clean, so it is not read", d.Device, fsType)
		}
		if !strings.HasPrefix(d.FSDevice, "/dev/") {
			return fmt.Errorf("%w: udev reports no filesystem node for %s", ErrDiskMount, d.Device)
		}
		reason, err := pendingLog(ctx, r.Runner, d.FSDevice, fsType)
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			return fmt.Errorf("%s could not be shown to have a clean %s log, so it is not read (%s)", d.Device, fsType, briefly(err))
		}
		if reason != "" {
			return fmt.Errorf("%s is not read: %s.%s", d.Device, reason, cleanStopAdvice)
		}
	}
	return withDisk(ctx, r.Mounter, d, fsType, filepath.Join(r.Dir, mountsDir, dirsMount), fn)
}

// TopLevelDirs implements DiskDirs.
func (r *DiskReader) TopLevelDirs(ctx context.Context, d disk.Disk) ([]string, error) {
	var names []string
	err := r.mount(ctx, d, func(root string) error {
		var err error
		names, err = topLevelDirs(root)
		return err
	})
	return names, err
}

// DirBytes implements DirSizer: the bytes of the regular files under the
// top-level directory name, by their metadata alone.
func (r *DiskReader) DirBytes(ctx context.Context, d disk.Disk, name string) (int64, error) {
	var total int64
	err := r.mount(ctx, d, func(root string) error {
		return filepath.WalkDir(filepath.Join(root, name), func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if e.Type().IsRegular() {
				info, err := e.Info()
				if err != nil {
					return err
				}
				total += info.Size()
			}
			return nil
		})
	})
	return total, err
}

// topLevelDirs lists the directories directly under root, sorted. A symlink is
// not a directory here: it is never followed.
func topLevelDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
