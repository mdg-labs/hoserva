package pool

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// SpaceStatter reports one mounted filesystem's capacity and free space
// straight from the kernel's own statfs(2) counters — never a directory
// walk (doc 09 §5: "nothing on a timer walks a data disk" applies here
// too, and cache.statfs=0, Options.render, already favours accuracy over
// stale numbers). The real implementation, StatfsSpaceStatter, wraps a
// single syscall; tests substitute FakeSpaceStatter, following the same
// "system-touching subsystem sits behind an interface with a scriptable
// fake" convention as disk.Provider and disk.Runner.
type SpaceStatter interface {
	StatSpace(ctx context.Context, path string) (SpaceStat, error)
}

// SpaceStat is one mount's statfs(2) result, already resolved to bytes.
// FreeBytes is the space available to a non-privileged writer (statfs's
// f_bavail, not f_bfree) — the number that actually determines whether a
// write succeeds, matching internal/api's own bootSpaceCheck.
type SpaceStat struct {
	TotalBytes int64
	FreeBytes  int64
}

// DiskSpace is one data disk's own statfs(2) reading, evaluated against
// the pool's configured minfreespace (doc 09 §1, §5). NearMinFreeSpace is
// true once FreeBytes reaches or drops below that threshold — the exact
// point at which mergerfs itself starts excluding the branch from
// create-policy placement (doc 09 §1's "minfreespace" table entry), so
// it is the one value worth flagging on rather than an arbitrary margin
// above it.
type DiskSpace struct {
	Path             string
	TotalBytes       int64
	FreeBytes        int64
	NearMinFreeSpace bool
}

// PoolSpace is what doc 09 §5 says must be shown everywhere space is
// displayed: pool total free, the largest single disk's free space — the
// real answer to "what is the biggest file I can write" — and the
// per-disk breakdown a pool-wide number alone hides.
type PoolSpace struct {
	Disks                []DiskSpace
	PoolFreeBytes        int64
	LargestDiskFreeBytes int64
	LargestDiskPath      string
}

// ComputePoolSpace reads every disk in diskPaths through statter — one
// statfs(2) call each, never a directory walk (doc 09 §5) — and reports
// pool-wide free space (the sum a `df` on the pool mount itself would
// show), the largest single disk's free space, and which disks are at or
// below minFreeSpace (mergerfs's own size-suffix syntax, Options.MinFreeSpace).
func ComputePoolSpace(ctx context.Context, statter SpaceStatter, diskPaths []string, minFreeSpace string) (PoolSpace, error) {
	if len(diskPaths) == 0 {
		return PoolSpace{}, ErrNoDataDisks
	}
	minFreeBytes, err := ParseMinFreeSpace(minFreeSpace)
	if err != nil {
		return PoolSpace{}, err
	}

	space := PoolSpace{Disks: make([]DiskSpace, 0, len(diskPaths))}
	for _, path := range diskPaths {
		stat, err := statter.StatSpace(ctx, path)
		if err != nil {
			return PoolSpace{}, fmt.Errorf("pool: reading free space on %s: %w", path, err)
		}
		d := DiskSpace{
			Path:             path,
			TotalBytes:       stat.TotalBytes,
			FreeBytes:        stat.FreeBytes,
			NearMinFreeSpace: stat.FreeBytes <= minFreeBytes,
		}
		space.Disks = append(space.Disks, d)
		space.PoolFreeBytes += d.FreeBytes
		if d.FreeBytes > space.LargestDiskFreeBytes || space.LargestDiskPath == "" {
			space.LargestDiskFreeBytes = d.FreeBytes
			space.LargestDiskPath = d.Path
		}
	}
	return space, nil
}

// ParseMinFreeSpace parses mergerfs's own minfreespace size-suffix syntax
// (Options.MinFreeSpace, e.g. "50G") into bytes: an optional trailing K,
// M, G or T (case-insensitive), 1024-based, matching disk.KB/MB/GB/TB. A
// bare number is bytes.
func ParseMinFreeSpace(s string) (int64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("pool: empty minfreespace value")
	}

	unit := int64(1)
	numPart := trimmed
	switch trimmed[len(trimmed)-1] {
	case 'k', 'K':
		unit, numPart = disk.KB, trimmed[:len(trimmed)-1]
	case 'm', 'M':
		unit, numPart = disk.MB, trimmed[:len(trimmed)-1]
	case 'g', 'G':
		unit, numPart = disk.GB, trimmed[:len(trimmed)-1]
	case 't', 'T':
		unit, numPart = disk.TB, trimmed[:len(trimmed)-1]
	}

	n, err := strconv.ParseInt(numPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pool: invalid minfreespace %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("pool: minfreespace %q must not be negative", s)
	}
	return n * unit, nil
}

// RebalanceSuggestion is doc 09 §1's remedy for its named sharp edge:
// under a path-preserving create policy, a disk at or below minfreespace
// can no longer accept a write toward a path it already holds — even
// while the rest of the pool has room — so this is offered as a
// rebalance suggestion (doc 09 §3), surfaced at the moment the condition
// is detected rather than acted on automatically.
type RebalanceSuggestion struct {
	ConstrainedDiskPath string
	Reason              string
}

// DetectRebalanceSuggestion reports whether space's own per-disk figures,
// combined with the share's create policy, show doc 09 §1's known sharp
// edge: a path-preserving policy — only KeepFoldersTogether in this
// package, since mergerfs's strictly path-preserving epmfs is never
// offered (Q11) — whose target disk has hit minfreespace while another
// disk still has meaningful room. KeepFoldersTogether (mspmfs) itself
// falls back to the parent path when this happens (S6, doc 09 §1) rather
// than failing outright, so this is a proactive nudge toward rebalancing
// the constrained disk, not a claim that the next write will fail.
func DetectRebalanceSuggestion(policy CreatePolicy, space PoolSpace) (RebalanceSuggestion, bool) {
	if policy != KeepFoldersTogether {
		return RebalanceSuggestion{}, false
	}
	for _, d := range space.Disks {
		if !d.NearMinFreeSpace {
			continue
		}
		if space.LargestDiskFreeBytes > 0 && d.Path != space.LargestDiskPath {
			return RebalanceSuggestion{
				ConstrainedDiskPath: d.Path,
				Reason: fmt.Sprintf(
					"%s is at or below minfreespace — a write under Keep folders together toward a path already on it can fail with no space left on device even though %s still has room; rebalancing evens this out",
					d.Path, space.LargestDiskPath,
				),
			}, true
		}
	}
	return RebalanceSuggestion{}, false
}
