package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// moverThresholdInterval is how often the daemon polls cache usage against
// cache.DefaultThresholdPercent (doc 09 §2's "threshold" trigger) — Q30's
// own "once a minute, or coarser" schedule-loop cadence, the same interval
// runScheduleLoop already ticks on. Each tick is a single statfs(2) call
// (pool.SpaceStatter), never a directory walk, and this is a coarse poll,
// never a continuous watcher (doc 09 §2: "A daemon watching for writes and
// moving them immediately defeats the purpose of a write cache").
const moverThresholdInterval = time.Minute

// moverThresholdRunner submits a TypeMover job when the cache disk is
// above cache.DefaultThresholdPercent full and no mover run is already
// queued or running — it reuses the exact same job.TypeMover registration
// (job.RunMover) the nightly chain and the manual `hoserva mover run` path
// submit through, never a second mover-invocation path.
type moverThresholdRunner struct {
	Scheduler *job.Scheduler
	Jobs      *job.Store
	Array     *store.ArrayStore
	Statter   pool.SpaceStatter
}

func runMoverThresholdLoop(ctx context.Context, r *moverThresholdRunner, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.tick(ctx); err != nil {
				log.Printf("hoservad: polling cache usage for the mover threshold: %v", err)
			}
		}
	}
}

func (r *moverThresholdRunner) tick(ctx context.Context) error {
	if r == nil || r.Scheduler == nil || r.Jobs == nil || r.Array == nil || r.Statter == nil {
		return nil
	}
	cachePath, err := r.cacheMountpoint(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNoArray) {
			// No array yet — nothing to relocate before create-array has
			// ever run (mirrors moverSharesFromStore's own "nothing to do
			// yet" treatment of ErrNoArray, mover.go).
			return nil
		}
		return fmt.Errorf("loading array topology: %w", err)
	}
	if cachePath == "" {
		// No cache disk configured — cache-then-move is not possible, so
		// there is nothing for the mover to move.
		return nil
	}

	stat, err := r.Statter.StatSpace(ctx, cachePath)
	if err != nil {
		return fmt.Errorf("reading cache disk usage at %s: %w", cachePath, err)
	}
	used := stat.TotalBytes - stat.FreeBytes
	if !cache.ThresholdExceeded(used, stat.TotalBytes, cache.DefaultThresholdPercent) {
		return nil
	}

	active, err := r.Jobs.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("listing active jobs: %w", err)
	}
	for _, j := range active {
		if j.Type == job.TypeMover {
			// A mover run is already queued or running — this tick's own
			// submission would only queue a redundant one behind it while
			// the threshold stays exceeded.
			return nil
		}
	}

	_, err = r.Scheduler.Submit(ctx, job.TypeMover, nil, nil)
	return err
}

// cacheMountpoint returns the array's cache disk mountpoint, or "" if the
// array has no cache disk configured.
func (r *moverThresholdRunner) cacheMountpoint(ctx context.Context) (string, error) {
	_, disks, err := r.Array.GetArray(ctx)
	if err != nil {
		return "", err
	}
	for _, d := range disks {
		if d.Role == store.ArrayRoleCache {
			return d.Mountpoint, nil
		}
	}
	return "", nil
}
