package container

import (
	"context"
	"sync"
	"time"
)

// DefaultWatchRetry is how long a Watcher waits before it reconnects to an
// Engine whose event stream ended or could not be opened — most often
// because Docker is not installed or not running yet.
const DefaultWatchRetry = 30 * time.Second

// Watcher turns the Engine's container event stream into Hub publications
// and unhealthy notifications. Nothing polls: a state change reaches the
// Hub when the Engine reports it.
type Watcher struct {
	Provider Provider
	Hub      *Hub
	// OnUnhealthy is called when a container's health changes to
	// unhealthy, once per transition. Nil skips it.
	OnUnhealthy func(StateChange)
	// Retry is the wait before reconnecting; zero means DefaultWatchRetry.
	Retry time.Duration

	mu     sync.Mutex
	health map[string]string
}

// Run watches until ctx is cancelled, reconnecting after every failure.
func (w *Watcher) Run(ctx context.Context) {
	retry := w.Retry
	if retry <= 0 {
		retry = DefaultWatchRetry
	}
	for {
		_ = w.Provider.Watch(ctx, w.handle)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

func (w *Watcher) handle(sc StateChange) {
	w.Hub.Publish(sc)
	w.mu.Lock()
	if w.health == nil {
		w.health = make(map[string]string)
	}
	prev := w.health[sc.ID]
	if sc.Health == "" {
		// A start, stop or die resets the container's health check.
		delete(w.health, sc.ID)
		w.mu.Unlock()
		return
	}
	w.health[sc.ID] = sc.Health
	w.mu.Unlock()
	if sc.Health == HealthUnhealthy && prev != HealthUnhealthy && w.OnUnhealthy != nil {
		w.OnUnhealthy(sc)
	}
}
