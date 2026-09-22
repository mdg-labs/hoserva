package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// spaceAlertInterval is how often the daemon checks the array's own
// per-disk free space against minfreespace and the KeepFoldersTogether
// ENOSPC sharp edge (doc 09 §1, §5) — Q30's cadence, the same interval
// runScheduleLoop already ticks on. Each tick is one statfs(2) call per
// data disk (pool.SpaceStatter), never a directory walk; doc 13 Q85
// records why that is spindown-safe.
const spaceAlertInterval = time.Minute

// spaceAlertNotifier is the two notify.Service publishers this runner
// fires on a transition into their condition (internal/notify.Service
// already implements this exact shape); tests use a fake so this package
// needn't construct a real notify.Service.
type spaceAlertNotifier interface {
	PublishDiskNearMinFreeSpace(ctx context.Context, diskPath string, freeBytes, totalBytes int64) error
	PublishRebalanceSuggested(ctx context.Context, constrainedDiskPath, reason string) error
}

// spaceAlertRunner polls the array's own per-disk free space and fires
// notify's disk_near_minfreespace event only on the transition into each
// condition — never once per tick while it stays there — mirroring
// moverThresholdRunner's own Array/Statter poll (mover_threshold.go), run
// alongside it at the same interval rather than folded in, since the two
// conditions are unrelated. In-memory state is enough: which disks were
// already alerted does not need to survive a restart.
type spaceAlertRunner struct {
	Array    *store.ArrayStore
	Statter  pool.SpaceStatter
	Notifier spaceAlertNotifier

	nearMinFreeSpace    map[string]bool
	rebalanceSuggestion string
}

func runSpaceAlertLoop(ctx context.Context, r *spaceAlertRunner, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.tick(ctx); err != nil {
				log.Printf("hoservad: checking array free space: %v", err)
			}
		}
	}
}

func (r *spaceAlertRunner) tick(ctx context.Context) error {
	if r == nil || r.Array == nil || r.Statter == nil || r.Notifier == nil {
		return nil
	}
	settings, disks, err := r.Array.GetArray(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNoArray) {
			// No array yet — nothing to check before create-array has
			// ever run, matching GetPool's own best-effort handling.
			return nil
		}
		return fmt.Errorf("loading array topology: %w", err)
	}

	var dataMounts []string
	for _, d := range disks {
		if d.Role == store.ArrayRoleData {
			dataMounts = append(dataMounts, d.Mountpoint)
		}
	}
	if len(dataMounts) == 0 {
		return nil
	}

	space, err := pool.ComputePoolSpace(ctx, r.Statter, dataMounts, settings.MinFreeSpace)
	if err != nil {
		return fmt.Errorf("computing pool space: %w", err)
	}

	r.tickNearMinFreeSpace(ctx, space)
	r.tickRebalanceSuggestion(ctx, settings, space)
	return nil
}

func (r *spaceAlertRunner) tickNearMinFreeSpace(ctx context.Context, space pool.PoolSpace) {
	if r.nearMinFreeSpace == nil {
		r.nearMinFreeSpace = make(map[string]bool, len(space.Disks))
	}
	for _, d := range space.Disks {
		was := r.nearMinFreeSpace[d.Path]
		if d.NearMinFreeSpace && !was {
			if err := r.Notifier.PublishDiskNearMinFreeSpace(ctx, d.Path, d.FreeBytes, d.TotalBytes); err != nil {
				log.Printf("hoservad: publishing disk-near-minfreespace alert for %s: %v", d.Path, err)
				// Leave nearMinFreeSpace[d.Path] as "was" (false) so the
				// next tick retries the publish instead of treating a
				// failed notification as delivered.
				continue
			}
		}
		r.nearMinFreeSpace[d.Path] = d.NearMinFreeSpace
	}
}

func (r *spaceAlertRunner) tickRebalanceSuggestion(ctx context.Context, settings store.ArraySettings, space pool.PoolSpace) {
	policy := pool.DefaultCreatePolicy
	if settings.CreatePolicy != "" {
		policy = pool.CreatePolicy(settings.CreatePolicy)
	}

	var suggestedPath string
	if suggestion, ok := pool.DetectRebalanceSuggestion(policy, space); ok {
		suggestedPath = suggestion.ConstrainedDiskPath
		if suggestedPath != r.rebalanceSuggestion {
			if err := r.Notifier.PublishRebalanceSuggested(ctx, suggestion.ConstrainedDiskPath, suggestion.Reason); err != nil {
				log.Printf("hoservad: publishing rebalance-suggested alert for %s: %v", suggestedPath, err)
				// Leave r.rebalanceSuggestion at its previous value so
				// the next tick retries the publish instead of treating
				// a failed notification as delivered.
				return
			}
		}
	}
	r.rebalanceSuggestion = suggestedPath
}
