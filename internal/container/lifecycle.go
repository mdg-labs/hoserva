package container

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Lifecycle is the business logic behind the /apps lifecycle operations
// (doc 04 §1): start, stop, restart, recreate, remove, logs and stats for
// any container the Provider can see. It treats every container the same,
// managed or not — nothing here reads or writes a stack, template or
// label, so a container Hoserva did not install is controlled without
// being modified.
type Lifecycle struct {
	Provider Provider
	// Hub receives a StateChange after every start, stop, restart and
	// recreate. Nil discards them.
	Hub *Hub
	// AppdataRoots returns the host directories Remove may delete under
	// when the caller asks for appdata deletion. Nil means there are none.
	AppdataRoots func(ctx context.Context) ([]string, error)
}

// RemoveResult reports what Remove deleted besides the container.
type RemoveResult struct {
	// DeletedPaths are the appdata directories deleted; empty unless the
	// caller asked for appdata deletion.
	DeletedPaths []string
}

// Start starts the container and returns its state afterwards.
func (l *Lifecycle) Start(ctx context.Context, id string) (Container, error) {
	return l.act(ctx, id, "starting", l.Provider.Start)
}

// Stop stops the container and returns its state afterwards.
func (l *Lifecycle) Stop(ctx context.Context, id string) (Container, error) {
	return l.act(ctx, id, "stopping", l.Provider.Stop)
}

// Restart restarts the container and returns its state afterwards.
func (l *Lifecycle) Restart(ctx context.Context, id string) (Container, error) {
	return l.act(ctx, id, "restarting", l.Provider.Restart)
}

// Recreate replaces the container with a freshly pulled, identically
// configured one (Provider.Recreate) and returns the replacement's state.
func (l *Lifecycle) Recreate(ctx context.Context, id string) (Container, error) {
	c, err := l.Provider.Inspect(ctx, id)
	if err != nil {
		return Container{}, err
	}
	if err := l.Provider.Recreate(ctx, c.ID); err != nil {
		return Container{}, fmt.Errorf("recreating container %q: %w", c.Name, err)
	}
	return l.settle(ctx, c.Name)
}

func (l *Lifecycle) act(ctx context.Context, id, verb string, do func(context.Context, string) error) (Container, error) {
	c, err := l.Provider.Inspect(ctx, id)
	if err != nil {
		return Container{}, err
	}
	if err := do(ctx, c.ID); err != nil {
		return Container{}, fmt.Errorf("%s container %q: %w", verb, c.Name, err)
	}
	return l.settle(ctx, c.ID)
}

// settle reads the container's state after an action and publishes it.
func (l *Lifecycle) settle(ctx context.Context, id string) (Container, error) {
	c, err := l.Provider.Inspect(ctx, id)
	if err != nil {
		return Container{}, fmt.Errorf("reading container state after the action: %w", err)
	}
	l.Hub.Publish(StateChange{ID: c.ID, Name: c.Name, State: c.State, Health: c.Health, At: time.Now().UTC()})
	return c, nil
}

// Remove removes a stopped container. Its appdata is deleted only when
// deleteAppdata is true — never as a side effect of anything else — and
// then only the bind-mount directories strictly inside an appdata root
// that no other container uses (planAppdataDeletion). Everything that can
// refuse the request is checked before the container is removed, and the
// directories are deleted only after the Engine has removed it, so a
// failed removal deletes nothing.
func (l *Lifecycle) Remove(ctx context.Context, id string, deleteAppdata bool) (RemoveResult, error) {
	c, err := l.Provider.Inspect(ctx, id)
	if err != nil {
		return RemoveResult{}, err
	}
	switch c.State {
	case "created", "exited", "dead":
	default:
		return RemoveResult{}, fmt.Errorf("removing container %q (state %s): %w", c.Name, c.State, ErrRunning)
	}

	var plan []string
	if deleteAppdata {
		if l.AppdataRoots == nil {
			return RemoveResult{}, ErrAppdataUnavailable
		}
		roots, err := l.AppdataRoots(ctx)
		if err != nil {
			return RemoveResult{}, fmt.Errorf("finding the appdata location: %w", err)
		}
		if len(roots) == 0 {
			return RemoveResult{}, ErrAppdataUnavailable
		}
		others, err := l.Provider.List(ctx)
		if err != nil {
			return RemoveResult{}, fmt.Errorf("listing containers to check for shared appdata: %w", err)
		}
		plan, err = planAppdataDeletion(c, others, roots)
		if err != nil {
			return RemoveResult{}, err
		}
	}

	// Once the Engine has removed the container the request cannot be
	// retried (the container is gone), so a client that disconnects from
	// here on must not leave the appdata half deleted.
	ctx = context.WithoutCancel(ctx)
	if err := l.Provider.Remove(ctx, c.ID, RemoveOptions{Volumes: deleteAppdata}); err != nil {
		return RemoveResult{}, fmt.Errorf("removing container %q: %w", c.Name, err)
	}
	if !deleteAppdata {
		return RemoveResult{}, nil
	}
	deleted, err := removeAppdataDirs(ctx, plan)
	if err != nil {
		return RemoveResult{DeletedPaths: deleted}, fmt.Errorf("container %q was removed but its appdata was not fully deleted: %w", c.Name, err)
	}
	return RemoveResult{DeletedPaths: deleted}, nil
}

// Logs returns the container's output; see Provider.Logs.
func (l *Lifecycle) Logs(ctx context.Context, id string, opts LogOptions) (io.ReadCloser, error) {
	return l.Provider.Logs(ctx, id, opts)
}

// Stats returns the container's current resource use; see Provider.Stats.
func (l *Lifecycle) Stats(ctx context.Context, id string) (Stats, error) {
	return l.Provider.Stats(ctx, id)
}
