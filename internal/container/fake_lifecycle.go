package container

import (
	"context"
	"io"
	"strings"
	"time"
)

// FakeCall records one lifecycle call made through a FakeProvider.
type FakeCall struct {
	Op string
	ID string
}

// FailOn scripts op ("start", "stop", "restart", "remove", "recreate",
// "logs", "stats", "reconcile") to return err for the container whose ID or name is
// id, or for every container when id is "". The container is left
// exactly as it was. A nil err clears the script.
func (f *FakeProvider) FailOn(op, id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures == nil {
		f.failures = make(map[string]error)
	}
	if err == nil {
		delete(f.failures, op+"\x00"+id)
		return
	}
	f.failures[op+"\x00"+id] = err
}

// Calls returns every lifecycle call made so far, in call order. A
// "remove" that also asked for the container's anonymous volumes is
// followed by a "remove-volumes" entry for the same container.
func (f *FakeProvider) Calls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// SetLogs scripts the text Logs returns for the container id.
func (f *FakeProvider) SetLogs(id, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logs == nil {
		f.logs = make(map[string]string)
	}
	f.logs[id] = text
}

// SetStats scripts what Stats returns for the container id.
func (f *FakeProvider) SetStats(id string, s Stats) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stats == nil {
		f.stats = make(map[string]Stats)
	}
	f.stats[id] = s
}

// Kill stops the container as an outside actor would (docker kill) and
// delivers the resulting state change to every Watch, exactly as the
// Engine's event stream does.
func (f *FakeProvider) Kill(id string) error {
	return f.transition(id, "", func(c *Container) {
		c.State = "exited"
		c.Status = "Exited (137) just now"
		c.Health = HealthNone
	})
}

// SetHealth changes the container's health as its health check would and
// delivers the change to every Watch.
func (f *FakeProvider) SetHealth(id, health string) error {
	return f.transition(id, health, func(c *Container) {
		c.Health = health
	})
}

func (f *FakeProvider) transition(id, health string, mutate func(*Container)) error {
	f.mu.Lock()
	i := f.indexLocked(id)
	if i < 0 {
		f.mu.Unlock()
		return ErrNotFound
	}
	mutate(&f.containers[i])
	change := changeOf(f.containers[i], health)
	f.mu.Unlock()
	f.emit(change)
	return nil
}

func changeOf(c Container, health string) StateChange {
	return StateChange{ID: c.ID, Name: c.Name, State: c.State, Health: health, At: time.Now().UTC()}
}

// emit never blocks: a Watch whose consumer is not keeping up loses the
// change, like a real event stream's slow reader.
func (f *FakeProvider) emit(sc StateChange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.watchers {
		select {
		case ch <- sc:
		default:
		}
	}
}

func (f *FakeProvider) indexLocked(id string) int {
	for i, c := range f.containers {
		if c.ID == id || c.Name == id {
			return i
		}
	}
	return -1
}

func (f *FakeProvider) actLocked(op, id string) (int, error) {
	f.calls = append(f.calls, FakeCall{Op: op, ID: id})
	if f.listErr != nil {
		return -1, f.listErr
	}
	i := f.indexLocked(id)
	if i < 0 {
		return -1, ErrNotFound
	}
	c := f.containers[i]
	for _, key := range []string{id, c.ID, c.Name, ""} {
		if err, ok := f.failures[op+"\x00"+key]; ok {
			return -1, err
		}
	}
	return i, nil
}

func (f *FakeProvider) setState(op, id, state, status string) error {
	f.mu.Lock()
	i, err := f.actLocked(op, id)
	if err != nil {
		f.mu.Unlock()
		return err
	}
	f.containers[i].State = state
	f.containers[i].Status = status
	change := changeOf(f.containers[i], "")
	f.mu.Unlock()
	f.emit(change)
	return nil
}

func (f *FakeProvider) Start(ctx context.Context, id string) error {
	return f.setState("start", id, "running", "Up 1 second")
}

func (f *FakeProvider) Stop(ctx context.Context, id string) error {
	return f.setState("stop", id, "exited", "Exited (0) just now")
}

func (f *FakeProvider) Restart(ctx context.Context, id string) error {
	return f.setState("restart", id, "running", "Up 1 second")
}

func (f *FakeProvider) Remove(ctx context.Context, id string, opts RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.actLocked("remove", id)
	if err != nil {
		return err
	}
	switch f.containers[i].State {
	case "running", "paused", "restarting":
		return ErrRunning
	}
	if opts.Volumes {
		f.calls = append(f.calls, FakeCall{Op: "remove-volumes", ID: id})
	}
	f.containers = append(f.containers[:i], f.containers[i+1:]...)
	return nil
}

// Recreate leaves the container as it is: a scripted failure models a
// pull or create that failed before anything was replaced.
func (f *FakeProvider) Recreate(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.actLocked("recreate", id)
	return err
}

// SetReconciliation scripts what Reconcile reports.
func (f *FakeProvider) SetReconciliation(r []Reconciliation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconciliation = append([]Reconciliation(nil), r...)
}

// Reconcile records a "reconcile" call and returns what SetReconciliation
// scripted; FailOn("reconcile", "", err) makes it fail instead.
func (f *FakeProvider) Reconcile(ctx context.Context) ([]Reconciliation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, FakeCall{Op: "reconcile"})
	if f.listErr != nil {
		return nil, f.listErr
	}
	if err, ok := f.failures["reconcile\x00"]; ok {
		return nil, err
	}
	return append([]Reconciliation(nil), f.reconciliation...), nil
}

func (f *FakeProvider) Logs(ctx context.Context, id string, opts LogOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.actLocked("logs", id)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(f.logs[f.containers[i].ID])), nil
}

func (f *FakeProvider) Stats(ctx context.Context, id string) (Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.actLocked("stats", id)
	if err != nil {
		return Stats{}, err
	}
	if f.containers[i].State != "running" {
		return Stats{}, ErrNotRunning
	}
	return f.stats[f.containers[i].ID], nil
}

func (f *FakeProvider) Watch(ctx context.Context, fn func(StateChange)) error {
	f.mu.Lock()
	if f.listErr != nil {
		err := f.listErr
		f.mu.Unlock()
		return err
	}
	if f.watchers == nil {
		f.watchers = make(map[chan StateChange]struct{})
	}
	ch := make(chan StateChange, 256)
	f.watchers[ch] = struct{}{}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.watchers, ch)
		f.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case sc := <-ch:
			fn(sc)
		}
	}
}
