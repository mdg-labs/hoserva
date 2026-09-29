package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"
)

// containerArrayStateName is the file in the state directory holding the
// containers `array stop` stopped and `array start` still owes a start.
const containerArrayStateName = "array-containers.json"

// appServices is everything the daemon builds around one
// container.Provider: the lifecycle service the /apps operations call, the
// array stop/start service, and the watcher that turns the Engine's event
// stream into container_state events and unhealthy notifications.
type appServices struct {
	Lifecycle *container.Lifecycle
	Array     *container.ArrayService
	Watcher   *container.Watcher

	// reconciled closes when reconcileContainersAtStart has finished; the
	// recreate job runs only after it.
	reconciled chan struct{}
}

// newContainers builds them over provider, or returns nil for a nil
// provider (a Docker client that could not even be constructed).
func newContainers(provider container.Provider, stateDir string, arrays *store.ArrayStore, notifier *notify.Service) *appServices {
	if provider == nil {
		return nil
	}
	hub := container.NewHub()
	c := &appServices{
		Lifecycle: &container.Lifecycle{
			Provider:     provider,
			Hub:          hub,
			AppdataRoots: appdataRoots(arrays),
		},
		Array: &container.ArrayService{
			Provider:  provider,
			StatePath: filepath.Join(stateDir, containerArrayStateName),
		},
		Watcher:    &container.Watcher{Provider: provider, Hub: hub},
		reconciled: make(chan struct{}),
	}
	if notifier != nil {
		c.Watcher.OnUnhealthy = func(sc container.StateChange) {
			name := sc.Name
			if name == "" {
				name = sc.ID
			}
			// A detached context: this runs from the event stream, not a
			// request, and the notification must not be lost to one.
			err := notifier.Publish(context.Background(), notify.EventContainerUnhealthy,
				fmt.Sprintf("Container %s is unhealthy", name),
				fmt.Sprintf("The health check of container %s is failing.", name))
			if err != nil {
				log.Printf("hoservad: publishing the unhealthy notification for container %s: %v", name, err)
			}
		}
	}
	return c
}

// containerHubOf is the hub the events endpoint subscribes to for
// container_state events: the one the Lifecycle publishes into and the
// Watcher forwards the Engine's events to. Nil when Docker is not wired.
func containerHubOf(handler *api.Handler) *container.Hub {
	if handler.Lifecycle == nil {
		return nil
	}
	return handler.Lifecycle.Hub
}

// arrayService is what ArraySequence.Services carries; nil when there is
// no Docker client, so the sequence has no container step to fail on.
func (c *appServices) arrayService() job.ArrayService {
	if c == nil {
		return nil
	}
	return c.Array
}

// containerRestoreInterval is how often the boot restore looks again while
// storage is not ready or Docker is not answering yet. Each look is a file
// read, an in-memory flag and a call to the Docker socket, never a data
// disk.
const containerRestoreInterval = 10 * time.Second

// restoreContainersAfterShutdown starts, in the background, the containers
// an update reboot or UPS shutdown stopped: those stops leave no persisted
// array stop, and dockerd does not restart a container the Engine stopped,
// whatever its restart policy. halted is the scheduler's maintenance mode
// (a persisted `array stop` is restored into it before this runs), and
// storageReady the storage target's readiness. The returned channel closes
// when the restore is over.
func restoreContainersAfterShutdown(ctx context.Context, c *appServices, halted, storageReady func() bool) <-chan struct{} {
	done := make(chan struct{})
	if c == nil {
		close(done)
		return done
	}
	c.Array.Halted = halted
	c.Array.StorageReady = storageReady
	go func() {
		defer close(done)
		if err := c.Array.RestoreAfterShutdown(ctx, containerRestoreInterval); err != nil && ctx.Err() == nil {
			log.Printf("hoservad: starting the containers the last shutdown stopped: %v", err)
		}
	}()
	return done
}

// reconcileWait is how long a recreate job waits for the start-up
// reconciliation before it gives up.
const reconcileWait = 2 * time.Minute

// reconcileContainersAtStart, in the background, undoes what a recreate
// that the last hoservad process did not finish left behind: a container
// still under its _hoserva-old or _hoserva-new name (container
// EngineClient.Reconcile). It can start the original again, so it waits
// for the array to be up like the boot restore does, and it tries again
// every interval while Docker is not answering yet. What it found is
// written to the daemon log. The returned channel closes when a pass has
// completed, or failed for a reason waiting will not fix — what could not
// be reconciled is then still there for the next start. It does not close
// when ctx ends: the daemon is going down.
func reconcileContainersAtStart(ctx context.Context, c *appServices, halted, storageReady func() bool, interval time.Duration) <-chan struct{} {
	if c == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if halted != nil && storageReady != nil && !halted() && storageReady() {
				reports, err := c.Lifecycle.Provider.Reconcile(ctx)
				for _, r := range reports {
					log.Printf("hoservad: container %s, left by an interrupted recreate: %s: %s", r.Name, r.Outcome, r.Detail)
				}
				if err == nil {
					close(c.reconciled)
					return
				}
				if !errors.Is(err, container.ErrUnavailable) {
					log.Printf("hoservad: reconciling containers left by an interrupted recreate: %v", err)
					close(c.reconciled)
					return
				}
			}
			timer.Reset(interval)
		}
	}()
	return c.reconciled
}

// awaitReconciled blocks until the start-up reconciliation has finished,
// the context ends, or reconcileWait has passed.
func (c *appServices) awaitReconciled(ctx context.Context) error {
	timer := time.NewTimer(reconcileWait)
	defer timer.Stop()
	select {
	case <-c.reconciled:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("the containers left by an interrupted recreate have not been reconciled yet — try again shortly")
	}
}

// wireContainers is what main.go calls to make container lifecycle
// reachable: the /apps operations (Handler.Lifecycle) and the recreate
// job. A test calls it too, rather than repeating the assignments.
// halted and storageReady are the same two signals the boot restore reads:
// with them Lifecycle refuses start, restart, recreate and removing with
// appdata while the array is stopped or its storage is not ready, and with
// either nil it refuses always (fail closed). admit is the hold array stop
// drains (arrayActionAdmit): start, restart and removing with appdata
// hold it from their array check to their last write.
func wireContainers(handler *api.Handler, registry *job.Registry, c *appServices, halted, storageReady func() bool, admit func() (func(), error)) {
	if c == nil {
		return
	}
	c.Lifecycle.Halted = halted
	c.Lifecycle.StorageReady = storageReady
	c.Lifecycle.Admit = admit
	handler.Lifecycle = c.Lifecycle
	// Recreate replaces the container only after everything that can fail
	// has succeeded, and puts the original back if a later step fails, so
	// it is not cancellable: a cancel between its steps would be the one
	// way to leave both containers half-swapped.
	// It also waits for reconcileContainersAtStart, so it never starts
	// while an interrupted recreate's leftovers are still being sorted out.
	registry.Register(job.TypeContainerRecreate, false, job.RunContainerRecreate(func(ctx context.Context, id string) error {
		if err := c.Lifecycle.RequireArrayRunning(); err != nil {
			return err
		}
		if err := c.awaitReconciled(ctx); err != nil {
			return err
		}
		_, err := c.Lifecycle.Recreate(ctx, id)
		return err
	}))
}

// arrayActionAdmit is Lifecycle.Admit over the scheduler: it takes the hold
// ArraySequence.Stop drains, and once maintenance mode has begun refuses
// with container.ErrArrayStopped, the same refusal Lifecycle gives a call
// that arrives on an array that is already stopped.
func arrayActionAdmit(s *job.Scheduler) func() (func(), error) {
	return func() (func(), error) {
		if err := s.BeginAppAction(); err != nil {
			return nil, fmt.Errorf("%w: it is in maintenance mode — start the array first", container.ErrArrayStopped)
		}
		return s.FinishAppAction, nil
	}
}

// appdataRoots is where Remove may delete a container's appdata, read from
// the stored array topology (container.CacheAppdataRoots). With no array or
// no cache disk there is none, and Remove refuses to delete appdata.
func appdataRoots(arrays *store.ArrayStore) func(ctx context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		_, disks, err := arrays.GetArray(ctx)
		if err != nil {
			if errors.Is(err, store.ErrNoArray) {
				return nil, nil
			}
			return nil, fmt.Errorf("loading array topology: %w", err)
		}
		return container.CacheAppdataRoots(disks), nil
	}
}
