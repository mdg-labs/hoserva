package pool

import (
	"fmt"
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
