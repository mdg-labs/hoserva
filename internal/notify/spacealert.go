package notify

import (
	"context"
	"fmt"
)

// PublishDiskNearMinFreeSpace is doc 03 §8.3's "any single disk near
// minfreespace" event source (doc 09 §1, §5): a caller that already
// computed per-disk free space (internal/pool.ComputePoolSpace) calls
// this once per disk it found at or below the configured minfreespace,
// so the per-disk alert fires independently of pool-wide capacity — "a
// pool that is 40% free can still refuse writes" (doc 09 §5).
func (s *Service) PublishDiskNearMinFreeSpace(ctx context.Context, diskPath string, freeBytes, totalBytes int64) error {
	title := fmt.Sprintf("Disk %s is near its free-space threshold", diskPath)
	message := fmt.Sprintf(
		"%s has %s free of %s — at or below the configured minfreespace threshold, so mergerfs excludes it from new-file placement until it frees up.",
		diskPath, formatGiB(freeBytes), formatGiB(totalBytes),
	)
	return s.Publish(ctx, EventDiskNearMinFreeSpace, title, message)
}

// PublishRebalanceSuggested surfaces doc 09 §1's ENOSPC-under-path-
// preserving-policy sharp edge (internal/pool.DetectRebalanceSuggestion)
// the moment it is detected, as doc 09 §3 asks — never acted on
// automatically. It reuses disk_near_minfreespace: the underlying cause
// doc 03 §8.3 lists is the same "disk near minfreespace" condition this
// elaborates on, and the fixed event catalog has no separate entry for a
// rebalance suggestion.
func (s *Service) PublishRebalanceSuggested(ctx context.Context, constrainedDiskPath, reason string) error {
	title := fmt.Sprintf("Rebalance suggested: %s is constraining new writes", constrainedDiskPath)
	return s.Publish(ctx, EventDiskNearMinFreeSpace, title, reason)
}

// formatGiB renders bytes as whole gibibytes, matching internal/api's own
// bootSpaceCheck — plenty of precision for a threshold alert, and it
// never claims a fractional accuracy statfs(2) doesn't give.
func formatGiB(bytes int64) string {
	return fmt.Sprintf("%d GiB", bytes/(1<<30))
}
