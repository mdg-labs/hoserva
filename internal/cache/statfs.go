package cache

import (
	"os"
	"path/filepath"
	"syscall"
)

// AvailableBytes reports the free bytes statfs(2) sees at path — the real
// implementation of Deps.Avail, used for doc 09 §2's space pre-check.
// It calls syscall.Statfs directly rather than adding a per-OS split,
// following internal/api/doctor.go's own precedent: Hoserva's production
// and CI targets are both Linux. path is resolved to its nearest existing
// ancestor first, so the check still reads the right filesystem's free
// space for a share directory mergerfs has not created on a given branch
// yet — the branch mount itself always exists even when the share's own
// subdirectory under it does not.
func AvailableBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(nearestExisting(path), &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func nearestExisting(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}
