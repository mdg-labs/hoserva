package pool

import (
	"context"
	"fmt"
	"syscall"
)

// StatfsSpaceStatter is the real SpaceStatter: a single statfs(2) call on
// path, nothing else. No directory is walked, so calling it never wakes a
// spun-down disk and costs no more than the kernel's own cached
// filesystem stats (doc 09 §5) — the same syscall internal/api's own
// bootSpaceCheck and internal/disk's own mountpoint checks already use.
type StatfsSpaceStatter struct{}

// StatSpace statfs(2)s path and reports its total size and the space
// available to a non-privileged writer.
func (StatfsSpaceStatter) StatSpace(ctx context.Context, path string) (SpaceStat, error) {
	if err := ctx.Err(); err != nil {
		return SpaceStat{}, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return SpaceStat{}, fmt.Errorf("pool: statfs %s: %w", path, err)
	}
	bsize := uint64(st.Bsize)
	return SpaceStat{
		TotalBytes: int64(st.Blocks * bsize),
		FreeBytes:  int64(st.Bavail * bsize),
	}, nil
}

var _ SpaceStatter = StatfsSpaceStatter{}
