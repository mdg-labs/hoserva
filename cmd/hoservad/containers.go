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
		Watcher: &container.Watcher{Provider: provider, Hub: hub},
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

// wireContainers is what main.go calls to make container lifecycle
// reachable: the /apps operations (Handler.Lifecycle) and the recreate
// job. A test calls it too, rather than repeating the assignments.
func wireContainers(handler *api.Handler, registry *job.Registry, c *appServices) {
	if c == nil {
		return
	}
	handler.Lifecycle = c.Lifecycle
	// Recreate replaces the container only after everything that can fail
	// has succeeded, and puts the original back if a later step fails, so
	// it is not cancellable: a cancel between its steps would be the one
	// way to leave both containers half-swapped.
	registry.Register(job.TypeContainerRecreate, false, job.RunContainerRecreate(func(ctx context.Context, id string) error {
		_, err := c.Lifecycle.Recreate(ctx, id)
		return err
	}))
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
