package container

import (
	"context"
	"fmt"
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
// "recreate-swap", "recreate-local", "started-at", "created-at", "logs", "stats", "reconcile") to return
// err for the container whose ID or name is id, or for every container when
// id is "". The container is left exactly as it was. "pull-image",
// "tag-image" and "untag-image" are scripted per image reference instead. A
// nil err clears the script.
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

// FakeStart is one start or restart of a container, with the image it ran
// when it started.
type FakeStart struct {
	Name    string
	ImageID string
}

// Starts returns every start and restart made so far, in order.
func (f *FakeProvider) Starts() []FakeStart {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeStart(nil), f.starts...)
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
	if state == "running" {
		f.starts = append(f.starts, FakeStart{Name: f.containers[i].Name, ImageID: f.containers[i].ImageID})
		f.markStartedLocked(f.containers[i])
	}
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

// SetPull scripts the image a pull of ref (a container's "repository:tag")
// brings in: the next Recreate of a container with that reference moves the
// tag to the image with this ID, which must have been added with AddImage,
// and the container then runs it. Without it a Recreate pulls nothing new.
func (f *FakeProvider) SetPull(ref, imageID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pulls == nil {
		f.pulls = make(map[string]string)
	}
	f.pulls[ref] = imageID
}

// containerRef is the reference a container's image is pulled by.
func containerRef(c Container) string {
	ref, err := ParseImageRef(c.Image, c.Tag)
	if err != nil {
		return c.Image + ":" + c.Tag
	}
	return ref.String()
}

// Recreate leaves the container as it is, with a scripted failure of op
// "recreate" modelling a pull or create that failed before anything was
// replaced. A scripted pull (SetPull) moves the tag before the swap, and a
// failure of op "recreate-swap" models a swap that fails after it: the
// container is unchanged and the tag has moved.
func (f *FakeProvider) Recreate(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.actLocked("recreate", id)
	if err != nil {
		return err
	}
	ref := containerRef(f.containers[i])
	pulled, ok := f.pulls[ref]
	if ok {
		if err := f.moveTagLocked(pulled, ref); err != nil {
			return err
		}
	}
	if err := f.failureLocked("recreate-swap", f.containers[i]); err != nil {
		return err
	}
	f.replaceLocked(i)
	if ok {
		f.containers[i].ImageID = pulled
		if f.containers[i].State == "running" {
			f.starts = append(f.starts, FakeStart{Name: f.containers[i].Name, ImageID: pulled})
		}
	}
	return nil
}

// replaceLocked models the Engine making a new container for a recreation:
// it has a new ID and creation time and has not started, unless the
// container it replaces ran, in which case the swap starts it.
func (f *FakeProvider) replaceLocked(i int) {
	old := f.containers[i].ID
	f.replaced++
	f.containers[i].ID = fmt.Sprintf("%s-r%d", old, f.replaced)
	delete(f.started, old)
	if f.created == nil {
		f.created = make(map[string]time.Time)
	}
	f.created[f.containers[i].ID] = f.nowLocked().UTC()
	if f.containers[i].State == "running" {
		f.markStartedLocked(f.containers[i])
	}
}

// PullImage records a "pull-image" call (ID is the reference) and, for a
// scripted pull (SetPull), moves the tag to the new image. FailOn can script
// it per reference.
func (f *FakeProvider) PullImage(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, FakeCall{Op: "pull-image", ID: ref})
	if f.listErr != nil {
		return f.listErr
	}
	if err := f.imageFailureLocked("pull-image", ref); err != nil {
		return err
	}
	if pulled, ok := f.pulls[ref]; ok {
		return f.moveTagLocked(pulled, ref)
	}
	return nil
}

// RecreateLocal replaces the container's image with whatever image holds the
// container's reference locally, pulling nothing.
func (f *FakeProvider) RecreateLocal(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, err := f.actLocked("recreate-local", id)
	if err != nil {
		return err
	}
	ref := containerRef(f.containers[i])
	for _, img := range f.images {
		for _, t := range img.RepoTags {
			if t == ref {
				f.replaceLocked(i)
				f.containers[i].ImageID = img.ID
				if f.containers[i].State == "running" {
					f.starts = append(f.starts, FakeStart{Name: f.containers[i].Name, ImageID: img.ID})
				}
				return nil
			}
		}
	}
	return fmt.Errorf("%w: %s", ErrImageNotFound, ref)
}

func (f *FakeProvider) failureLocked(op string, c Container) error {
	for _, key := range []string{c.ID, c.Name, ""} {
		if err, ok := f.failures[op+"\x00"+key]; ok {
			return err
		}
	}
	return nil
}

// moveTagLocked tags the image with this ID as ref, taking the tag off any
// other image.
func (f *FakeProvider) moveTagLocked(imageID, ref string) error {
	target := -1
	for i, img := range f.images {
		if img.ID == imageID {
			target = i
		}
	}
	if target < 0 {
		return fmt.Errorf("%w: %s", ErrImageNotFound, imageID)
	}
	for i := range f.images {
		kept := f.images[i].RepoTags[:0:0]
		for _, t := range f.images[i].RepoTags {
			if t != ref {
				kept = append(kept, t)
			}
		}
		f.images[i].RepoTags = kept
	}
	f.images[target].RepoTags = append(f.images[target].RepoTags, ref)
	return nil
}

// TagImage records a "tag-image" call (ID is the reference); FailOn can
// script it per reference.
func (f *FakeProvider) TagImage(ctx context.Context, imageID, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, FakeCall{Op: "tag-image", ID: ref})
	if err := f.imageFailureLocked("tag-image", ref); err != nil {
		return err
	}
	return f.moveTagLocked(imageID, ref)
}

// UntagImage records an "untag-image" call (ID is the reference). Like the
// Engine, it refuses with ErrImageInUse to remove the last reference of an
// image a container runs.
func (f *FakeProvider) UntagImage(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, FakeCall{Op: "untag-image", ID: ref})
	if err := f.imageFailureLocked("untag-image", ref); err != nil {
		return err
	}
	for i, img := range f.images {
		for j, t := range img.RepoTags {
			if t != ref {
				continue
			}
			if len(img.RepoTags) == 1 {
				for _, c := range f.containers {
					if c.ImageID == img.ID {
						return fmt.Errorf("%w: %s", ErrImageInUse, c.Name)
					}
				}
			}
			f.images[i].RepoTags = append(append([]string(nil), img.RepoTags[:j]...), img.RepoTags[j+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrImageNotFound, ref)
}

func (f *FakeProvider) imageFailureLocked(op, ref string) error {
	for _, key := range []string{ref, ""} {
		if err, ok := f.failures[op+"\x00"+key]; ok {
			return err
		}
	}
	return nil
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

// SetClock scripts the time a start records as the container's start time;
// without it that is the real time.
func (f *FakeProvider) SetClock(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = now
}

// SetStartedAt scripts when the Engine last started the container, as if it
// had run then. A container nothing started has the zero time.
func (f *FakeProvider) SetStartedAt(id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.indexLocked(id)
	if i < 0 {
		return ErrNotFound
	}
	if f.started == nil {
		f.started = make(map[string]time.Time)
	}
	f.started[f.containers[i].ID] = at.UTC()
	return nil
}

func (f *FakeProvider) nowLocked() time.Time {
	if f.clock != nil {
		return f.clock()
	}
	return time.Now()
}

func (f *FakeProvider) markStartedLocked(c Container) {
	if f.started == nil {
		f.started = make(map[string]time.Time)
	}
	f.started[c.ID] = f.nowLocked().UTC()
}

// SetCreatedAt scripts when the Engine created the container. A container
// nothing scripted and no recreation made was created at the Unix epoch.
func (f *FakeProvider) SetCreatedAt(id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.indexLocked(id)
	if i < 0 {
		return ErrNotFound
	}
	if f.created == nil {
		f.created = make(map[string]time.Time)
	}
	f.created[f.containers[i].ID] = at.UTC()
	return nil
}

// CreatedAt returns when a Recreate or RecreateLocal made the container, or
// what SetCreatedAt scripted. FailOn("created-at", …) scripts an error.
func (f *FakeProvider) CreatedAt(ctx context.Context, id string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return time.Time{}, f.listErr
	}
	i := f.indexLocked(id)
	if i < 0 {
		return time.Time{}, ErrNotFound
	}
	if err := f.failureLocked("created-at", f.containers[i]); err != nil {
		return time.Time{}, err
	}
	if at, ok := f.created[f.containers[i].ID]; ok {
		return at, nil
	}
	return time.Unix(0, 0).UTC(), nil
}

// StartedAt returns when a Start, Restart or running Recreate last ran the
// container, or what SetStartedAt scripted. FailOn("started-at", …) scripts
// an error.
func (f *FakeProvider) StartedAt(ctx context.Context, id string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return time.Time{}, f.listErr
	}
	i := f.indexLocked(id)
	if i < 0 {
		return time.Time{}, ErrNotFound
	}
	if err := f.failureLocked("started-at", f.containers[i]); err != nil {
		return time.Time{}, err
	}
	return f.started[f.containers[i].ID], nil
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
