package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ArrayService stops every running container before the array unmounts
// and starts the ones it stopped when the array starts again (doc 02 §4:
// "a container holding a file open blocks the unmount", and a container
// that runs against an unmounted pool writes onto the boot device). It
// satisfies job.ArrayService without importing internal/job.
//
// The containers to start again are remembered in StatePath, so the list
// survives a daemon restart while the array is stopped. Only a Start that
// starts all of them, or RestoreAfterShutdown after a shutdown that left no
// persisted array stop, empties it.
type ArrayService struct {
	Provider Provider
	// StatePath is a file in Hoserva's own state directory.
	StatePath string
	// Halted reports whether a persisted `array stop` is in force. The
	// boot restore never starts a container while it is. Nil means no.
	Halted func() bool
	// StorageReady reports whether the storage target is ready, so the
	// boot restore does not start a container before its data is
	// mounted. Nil means ready.
	StorageReady func() bool

	mu sync.Mutex
}

// arrayState is the file StatePath holds: container names, which — unlike
// IDs — survive a recreate.
type arrayState struct {
	Containers []string `json:"containers"`
}

func (a *ArrayService) Name() string { return "Docker containers" }

// Stop stops every container that is not already stopped, or returns an
// error naming each one that would not stop — which holds the whole
// array stop up, before anything unmounts. The containers to start again
// are written down before the first one is stopped, and added to what an
// earlier failed attempt already wrote down, so a retry still remembers
// the containers that attempt had stopped.
//
// A Docker Engine that is not installed or not reachable has nothing
// running to stop (doc 04 §3: hoservad starts regardless), so Stop
// returns nil for it.
func (a *ArrayService) Stop(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	all, err := a.Provider.List(ctx)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return nil
		}
		return fmt.Errorf("listing containers: %w", err)
	}
	var live []string
	for _, c := range all {
		switch c.State {
		case "running", "paused", "restarting":
			live = append(live, c.Name)
		}
	}
	if len(live) == 0 {
		return nil
	}

	prev, err := a.load()
	if err != nil {
		return err
	}
	if err := a.save(arrayState{Containers: union(prev.Containers, live)}); err != nil {
		return err
	}

	var failures []error
	for _, name := range live {
		if err := a.Provider.Stop(ctx, name); err != nil {
			failures = append(failures, fmt.Errorf("stopping container %q: %w", name, err))
		}
	}
	return errors.Join(failures...)
}

// Start starts the containers Stop stopped. A container that has since
// been removed is dropped from the list. If any container will not start,
// the ones this call did start are stopped again — the array is about to
// be rolled back and unmounted, and a container must not run against an
// unmounted pool — and the error says so if that fails too. Every
// container that was not started stays on the list.
func (a *ArrayService) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	st, err := a.load()
	if err != nil {
		return err
	}
	if len(st.Containers) == 0 {
		return nil
	}

	owed := map[string]bool{}
	var started []string
	var failures []error
	for _, name := range st.Containers {
		owed[name] = true
		c, err := a.Provider.Inspect(ctx, name)
		if errors.Is(err, ErrNotFound) {
			delete(owed, name)
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("finding container %q: %w", name, err))
			continue
		}
		if err := a.Provider.Start(ctx, c.ID); err != nil {
			failures = append(failures, fmt.Errorf("starting container %q: %w", name, err))
			continue
		}
		started = append(started, name)
		delete(owed, name)
	}

	if len(failures) == 0 {
		return a.clear()
	}
	undoCtx := context.WithoutCancel(ctx)
	for _, name := range started {
		owed[name] = true
		if err := a.Provider.Stop(undoCtx, name); err != nil {
			failures = append(failures, fmt.Errorf("stopping container %q again after a failed start: %w", name, err))
		}
	}
	remaining := make([]string, 0, len(owed))
	for name := range owed {
		remaining = append(remaining, name)
	}
	sort.Strings(remaining)
	if err := a.save(arrayState{Containers: remaining}); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// RestoreAfterShutdown starts the containers a shutdown stopped, at the
// next boot. A reboot or UPS shutdown stops them through the Engine
// without a persisted array stop, and dockerd does not bring a container
// stopped that way back on its own, even with restart: unless-stopped. It
// polls every interval, until the list is empty, a persisted array stop is
// in force (`array start` then owes the containers), a container will not
// start, or ctx ends. Storage not being ready and Docker not answering yet
// are waited out; they are what a boot looks like.
func (a *ArrayService) RestoreAfterShutdown(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		retry, err := a.restoreOnce(ctx)
		if !retry {
			return err
		}
		timer.Reset(interval)
	}
}

// restoreOnce makes one attempt of RestoreAfterShutdown and reports
// whether to try again later. Every container that starts, or no longer
// exists, leaves the list; the others stay on it.
func (a *ArrayService) restoreOnce(ctx context.Context) (retry bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	st, err := a.load()
	if err != nil {
		return false, err
	}
	if len(st.Containers) == 0 {
		return false, nil
	}
	if a.Halted != nil && a.Halted() {
		return false, nil
	}
	if a.StorageReady != nil && !a.StorageReady() {
		return true, nil
	}

	var remaining []string
	var failures []error
	for i, name := range st.Containers {
		c, err := a.Provider.Inspect(ctx, name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err == nil {
			err = a.Provider.Start(ctx, c.ID)
		}
		if errors.Is(err, ErrUnavailable) {
			retry = true
			remaining = append(remaining, st.Containers[i:]...)
			break
		}
		if err != nil {
			remaining = append(remaining, name)
			failures = append(failures, fmt.Errorf("starting container %q after the shutdown: %w", name, err))
		}
	}

	if len(remaining) == 0 {
		return false, errors.Join(append(failures, a.clear())...)
	}
	if err := a.save(arrayState{Containers: remaining}); err != nil {
		failures = append(failures, err)
	}
	return retry, errors.Join(failures...)
}

func union(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, s := range append(append([]string(nil), a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func (a *ArrayService) load() (arrayState, error) {
	raw, err := os.ReadFile(a.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return arrayState{}, nil
	}
	if err != nil {
		return arrayState{}, fmt.Errorf("reading the list of stopped containers: %w", err)
	}
	var st arrayState
	if err := json.Unmarshal(raw, &st); err != nil {
		return arrayState{}, fmt.Errorf("reading the list of stopped containers %s: %w", a.StatePath, err)
	}
	return st, nil
}

func (a *ArrayService) clear() error {
	if err := os.Remove(a.StatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing the list of stopped containers: %w", err)
	}
	return syncDir(filepath.Dir(a.StatePath))
}

// save replaces the state file atomically and durably.
func (a *ArrayService) save(st arrayState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encoding the list of stopped containers: %w", err)
	}
	dir := filepath.Dir(a.StatePath)
	tmp, err := os.CreateTemp(dir, ".containers-*.tmp")
	if err != nil {
		return fmt.Errorf("writing the list of stopped containers: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing the list of stopped containers: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing the list of stopped containers: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the list of stopped containers: %w", err)
	}
	if err := os.Rename(tmp.Name(), a.StatePath); err != nil {
		return fmt.Errorf("writing the list of stopped containers: %w", err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	return nil
}
