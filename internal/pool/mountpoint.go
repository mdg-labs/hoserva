package pool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// isMountpoint reports whether path is currently a mount point: its
// device ID differs from its parent directory's, the same test
// `mountpoint(1)` and internal/disk's own (unexported) isMountpoint use
// for physical mounts. It does no exec and touches nothing but a stat of
// path and its parent, so Mounter.Mount and Mounter.Unmount can call it
// unconditionally, even outside the lab.
// IsMounted reports whether path is currently a mount point; a path
// that does not exist is not one.
func IsMounted(path string) bool {
	mounted, err := isMountpoint(path)
	return err == nil && mounted
}

// IsMountedConfirmed is IsMounted's tri-state form: a path that genuinely
// does not exist (os.Stat returns fs.ErrNotExist) is confirmed not
// mounted, exactly as IsMounted already treats it, and reported as
// (false, nil). Any other stat failure — in particular ENOTCONN, what a
// dead FUSE endpoint's own mount point reports once its serving process
// has exited without unmounting — cannot tell "live" from "gone", and is
// returned instead of being silently read as "not mounted" the way
// IsMounted's single bool would (#365).
func IsMountedConfirmed(path string) (bool, error) {
	mounted, err := isMountpoint(path)
	if err == nil {
		return mounted, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

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
		return 0, fmt.Errorf("pool: cannot determine device id for %s", path)
	}
	return uint64(st.Dev), nil
}
