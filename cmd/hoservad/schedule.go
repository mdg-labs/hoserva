package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/mdg-labs/hoserva/internal/acme"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// scheduleTickInterval is how often the daemon checks persisted schedules
// against the clock (Q30). The chain starts at a clock minute, never a
// second; this loop reads SQLite and submits jobs — it never walks a
// data disk.
const scheduleTickInterval = time.Minute

// diffGuardHolder holds the maintenance chain's own job.DiffGuard behind
// a mutex so main.go's parityRegistrar can set it once — either at
// startup or from the ArrayReady hook after a live array creation, from
// the create-array job's own goroutine (#265) — while runScheduleLoop's
// own goroutine reads it on every tick, the same concurrency shape
// Handler.SetArray/CurrentArray already established for Handler.Array
// (#263).
type diffGuardHolder struct {
	mu    sync.RWMutex
	guard job.DiffGuard
}

func (g *diffGuardHolder) set(guard job.DiffGuard) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.guard = guard
}

func (g *diffGuardHolder) get() job.DiffGuard {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.guard
}

type scheduleRunner struct {
	Schedules *api.ScheduleService
	Scheduler *job.Scheduler
	Guard     *diffGuardHolder
	Backup    job.ConfigBackup
	Notifier  job.ChainNotifier
	ACME      *acme.Service
	Jobs      *job.Store
}

func runScheduleLoop(ctx context.Context, r *scheduleRunner, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.tick(ctx); err != nil {
				log.Printf("hoservad: running due maintenance chain: %v", err)
			}
		}
	}
}

func (r *scheduleRunner) tick(ctx context.Context) error {
	if err := r.tickChain(ctx); err != nil {
		return err
	}
	return r.tickACME(ctx)
}

func (r *scheduleRunner) tickChain(ctx context.Context) error {
	if r == nil || r.Schedules == nil || r.Scheduler == nil || r.Guard == nil {
		return nil
	}
	guard := r.Guard.get()
	if guard == nil {
		return nil
	}
	claimed, err := r.Schedules.ClaimDueChain(ctx)
	if err != nil {
		return err
	}
	if claimed == nil {
		return nil
	}
	weekly := claimed.At.In(claimed.Location).Weekday() == time.Weekday(claimed.Settings.WeeklyScrubDay)
	chain := &job.MaintenanceChain{
		Scheduler: r.Scheduler,
		Guard:     guard,
		Backup:    r.Backup,
		Notifier:  r.Notifier,
		Enabled:   claimed.Settings.Enabled,
		Weekly:    weekly,
	}
	_, err = chain.Run(ctx)
	return err
}

type scheduleNotifier struct {
	svc *notify.Service
}

func (n *scheduleNotifier) NotifyGuardBlocked(ctx context.Context) {
	if n == nil || n.svc == nil {
		return
	}
	err := n.svc.Publish(ctx, notify.EventSyncBlockedThreshold,
		"Nightly sync held back",
		"The threshold guard blocked the nightly maintenance chain after the diff. Parity was not written.")
	if err != nil {
		log.Printf("hoservad: notifying blocked threshold guard: %v", err)
	}
}
