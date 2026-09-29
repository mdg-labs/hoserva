package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrArrayStopped is returned by Start, Restart and Recreate, and by Remove
// when it is asked to delete appdata, while the array is stopped
// (maintenance mode) or its storage is not ready: a container started then
// binds /mnt/user and /mnt/cache paths that are empty directories on the
// boot device, writes into them, and the data disappears under the pool
// when it mounts (doc 02 §1); appdata deletion finds the cache disk's
// directories missing and deletes nothing.
var ErrArrayStopped = errors.New("container: the array is stopped")

// ErrArrayStateUnknown is returned by the same actions when the Lifecycle
// has no way to read the array's state, so the refusal fails closed
// instead of acting on a guess.
var ErrArrayStateUnknown = errors.New("container: the array state cannot be read")

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
	// Halted reports whether the array is stopped (maintenance mode) and
	// StorageReady whether the storage target is ready — the same two
	// signals ArrayService's boot restore reads. Start, Restart and
	// Recreate need both: a nil func is an unreadable array state, and
	// refuses.
	Halted       func() bool
	StorageReady func() bool
	// Admit takes the hold that array stop drains before it lists the
	// running containers: Start, Restart and Remove with deleteAppdata
	// call it before their array check and call the returned release when
	// they return, so a call that passed its check is finished before the
	// array stops, and one arriving after maintenance began is refused
	// with an error wrapping ErrArrayStopped. Nil means no hold.
	Admit func() (release func(), err error)
}

func (l *Lifecycle) admit() (func(), error) {
	if l.Admit == nil {
		return func() {}, nil
	}
	return l.Admit()
}

// RequireArrayRunning returns nil only when the array is running and its
// storage ready — the one condition under which a container may be
// started or its appdata deleted. Start, Restart, Recreate and Remove with
// deleteAppdata call it before any Engine call, so a container's bind
// mounts are never resolved against an unmounted pool even when dockerd is
// reachable; the API also calls it, to refuse a recreate before queueing a
// job that would fail.
func (l *Lifecycle) RequireArrayRunning() error {
	if l.Halted == nil || l.StorageReady == nil {
		return ErrArrayStateUnknown
	}
	if l.Halted() {
		return fmt.Errorf("%w: it is in maintenance mode — start the array first", ErrArrayStopped)
	}
	if !l.StorageReady() {
		return fmt.Errorf("%w: its storage is not ready — wait for the array to come up", ErrArrayStopped)
	}
	return nil
}

// RemoveResult reports what Remove deleted besides the container.
type RemoveResult struct {
	// DeletedPaths are the appdata directories deleted; empty unless the
	// caller asked for appdata deletion.
	DeletedPaths []string
}

// Start starts the container and returns its state afterwards.
func (l *Lifecycle) Start(ctx context.Context, id string) (Container, error) {
	release, err := l.admit()
	if err != nil {
		return Container{}, err
	}
	defer release()
	if err := l.RequireArrayRunning(); err != nil {
		return Container{}, err
	}
	return l.act(ctx, id, "starting", l.Provider.Start)
}

// Stop stops the container and returns its state afterwards.
func (l *Lifecycle) Stop(ctx context.Context, id string) (Container, error) {
	return l.act(ctx, id, "stopping", l.Provider.Stop)
}

// Restart restarts the container and returns its state afterwards.
func (l *Lifecycle) Restart(ctx context.Context, id string) (Container, error) {
	release, err := l.admit()
	if err != nil {
		return Container{}, err
	}
	defer release()
	if err := l.RequireArrayRunning(); err != nil {
		return Container{}, err
	}
	return l.act(ctx, id, "restarting", l.Provider.Restart)
}

// Recreate replaces the container with a freshly pulled, identically
// configured one (Provider.Recreate) and returns the replacement's state.
// It checks the array again itself, not only when the job was queued: the
// job runs later, and a replacement of a running container is started.
func (l *Lifecycle) Recreate(ctx context.Context, id string) (Container, error) {
	if err := l.RequireArrayRunning(); err != nil {
		return Container{}, err
	}
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
//
// Deleting appdata needs the array running (RequireArrayRunning), checked
// before anything else: with the array stopped the cache disk is not
// mounted, every planned directory is missing, and removing the container
// would report success while the real appdata stays on the disk. The hold
// Admit takes lasts until the directories are deleted, so array stop
// cannot unmount the cache in between. A remove that keeps appdata never
// needs the array.
func (l *Lifecycle) Remove(ctx context.Context, id string, deleteAppdata bool) (RemoveResult, error) {
	if deleteAppdata {
		release, err := l.admit()
		if err != nil {
			return RemoveResult{}, err
		}
		defer release()
		if err := l.RequireArrayRunning(); err != nil {
			return RemoveResult{}, err
		}
	}
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
