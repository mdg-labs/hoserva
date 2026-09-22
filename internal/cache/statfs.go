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

// DiskUsage is one branch's own filesystem usage — PlanRebalance's own
// target-selection input (doc 09 §3, doc 09 §5): finding a share's most-
// and least-full disk needs each branch's fill level as a whole, not
// merely whether one file's worth of bytes fits (AvailableBytes' own,
// narrower question).
type DiskUsage struct {
	TotalBytes int64
	FreeBytes  int64
}

// UsedPercent is the fraction of TotalBytes currently used, as a
// percentage — PlanRebalance's own skew measure between two branches.
// TotalBytes<=0 (a filesystem this call could not measure) reports 0
// rather than dividing by zero: an unmeasurable disk is not evidence
// that it is full.
func (u DiskUsage) UsedPercent() float64 {
	if u.TotalBytes <= 0 {
		return 0
	}
	return float64(u.TotalBytes-u.FreeBytes) / float64(u.TotalBytes) * 100
}

// UsageBytes reports the real implementation of Deps.Usage: statfs(2)'s
// own total and available byte counts at path, resolved to its nearest
// existing ancestor the same way AvailableBytes is.
func UsageBytes(path string) (DiskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(nearestExisting(path), &st); err != nil {
		return DiskUsage{}, err
	}
	return DiskUsage{
		TotalBytes: int64(st.Blocks) * int64(st.Bsize),
		FreeBytes:  int64(st.Bavail) * int64(st.Bsize),
	}, nil
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
