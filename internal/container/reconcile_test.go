package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	dockercontainer "github.com/moby/moby/api/types/container"
	dockermount "github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"
)

// stateEngine is an Engine that keeps containers between calls: names,
// running state and the timestamps the Engine records. It can be
// "killed" before a named mutating call, after which every mutating call
// fails until it is revived — what a hoservad process dying between two
// steps of Recreate leaves behind, including a rollback that cannot run.
// Everything it does not implement panics (nil embedded interface).
type stateEngine struct {
	engineAPI

	mu         sync.Mutex
	containers []*stateContainer
	tick       int64
	killBefore string
	dead       bool
	calls      []string
	fail       map[string]error
	removed    []string
}

type stateContainer struct {
	name string
	info dockercontainer.InspectResponse
}

var stateEpoch = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func (e *stateEngine) stamp() string {
	e.tick++
	return stateEpoch.Add(time.Duration(e.tick) * time.Second).Format(time.RFC3339Nano)
}

const neverStamp = "0001-01-01T00:00:00Z"

func newStateEngine(running bool) *stateEngine {
	e := &stateEngine{fail: map[string]error{}}
	state := &dockercontainer.State{Status: dockercontainer.StateCreated, StartedAt: neverStamp, FinishedAt: neverStamp}
	created := e.stamp()
	if running {
		state.Status = dockercontainer.StateRunning
		state.Running = true
		state.StartedAt = e.stamp()
	} else {
		state.Status = dockercontainer.StateExited
		state.StartedAt = e.stamp()
		state.FinishedAt = e.stamp()
	}
	e.containers = []*stateContainer{{
		name: "jellyfin",
		info: dockercontainer.InspectResponse{
			ID:      oldContainerID,
			Name:    "/jellyfin",
			Created: created,
			State:   state,
			HostConfig: &dockercontainer.HostConfig{
				Binds:  []string{"/mnt/cache/appdata/jellyfin:/config"},
				Mounts: []dockermount.Mount{{Type: dockermount.TypeVolume, Source: "jellyfin-cache", Target: "/cache"}},
			},
			Config: &dockercontainer.Config{
				Image:    "lscr.io/linuxserver/jellyfin:10.9.7",
				Hostname: oldContainerID[:12],
				Labels:   map[string]string{"maintainer": "someone"},
			},
			Mounts: []dockercontainer.MountPoint{
				{Type: dockermount.TypeBind, Source: "/mnt/cache/appdata/jellyfin", Destination: "/config", RW: true},
				{Type: dockermount.TypeVolume, Name: "jellyfin-cache", Destination: "/cache", RW: true},
			},
			NetworkSettings: &dockercontainer.NetworkSettings{},
		},
	}}
	return e
}

func (e *stateEngine) byRef(ref string) *stateContainer {
	for _, c := range e.containers {
		if c.info.ID == ref || c.name == ref {
			return c
		}
	}
	return nil
}

func (e *stateEngine) label(id string) string {
	switch id {
	case oldContainerID:
		return "old"
	case newContainerID:
		return "new"
	}
	return id
}

// mutate records op and applies it unless the engine is dead, dies right
// before it, or the test scripted a failure for it.
func (e *stateEngine) mutate(op string, apply func() error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.dead && e.killBefore == op {
		e.dead = true
	}
	if e.dead {
		return errors.New("the Engine connection was lost")
	}
	e.calls = append(e.calls, op)
	if err := e.fail[op]; err != nil {
		return err
	}
	return apply()
}

func (e *stateEngine) ContainerList(ctx context.Context, o dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var items []dockercontainer.Summary
	for _, c := range e.containers {
		items = append(items, dockercontainer.Summary{ID: c.info.ID, Names: []string{"/" + c.name}, State: c.info.State.Status})
	}
	return dockerclient.ContainerListResult{Items: items}, nil
}

func (e *stateEngine) ContainerInspect(ctx context.Context, id string, o dockerclient.ContainerInspectOptions) (dockerclient.ContainerInspectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.byRef(id)
	if c == nil {
		return dockerclient.ContainerInspectResult{}, fmt.Errorf("no such container: %s", id)
	}
	info := c.info
	info.Name = "/" + c.name
	st := *c.info.State
	info.State = &st
	return dockerclient.ContainerInspectResult{Container: info}, nil
}

func (e *stateEngine) ImagePull(ctx context.Context, ref string, o dockerclient.ImagePullOptions) (dockerclient.ImagePullResponse, error) {
	return pullStream{ReadCloser: io.NopCloser(strings.NewReader(""))}, nil
}

func (e *stateEngine) ContainerCreate(ctx context.Context, o dockerclient.ContainerCreateOptions) (dockerclient.ContainerCreateResult, error) {
	err := e.mutate("create "+o.Name, func() error {
		e.containers = append(e.containers, &stateContainer{
			name: o.Name,
			info: dockercontainer.InspectResponse{
				ID:         newContainerID,
				Created:    e.stamp(),
				State:      &dockercontainer.State{Status: dockercontainer.StateCreated, StartedAt: neverStamp, FinishedAt: neverStamp},
				Config:     o.Config,
				HostConfig: o.HostConfig,
			},
		})
		return nil
	})
	return dockerclient.ContainerCreateResult{ID: newContainerID}, err
}

func (e *stateEngine) ContainerStop(ctx context.Context, id string, o dockerclient.ContainerStopOptions) (dockerclient.ContainerStopResult, error) {
	return dockerclient.ContainerStopResult{}, e.mutate("stop "+e.label(id), func() error {
		c := e.byRef(id)
		c.info.State.Running = false
		c.info.State.Status = dockercontainer.StateExited
		c.info.State.FinishedAt = e.stamp()
		return nil
	})
}

func (e *stateEngine) ContainerStart(ctx context.Context, id string, o dockerclient.ContainerStartOptions) (dockerclient.ContainerStartResult, error) {
	return dockerclient.ContainerStartResult{}, e.mutate("start "+e.label(id), func() error {
		c := e.byRef(id)
		c.info.State.Running = true
		c.info.State.Status = dockercontainer.StateRunning
		c.info.State.StartedAt = e.stamp()
		return nil
	})
}

func (e *stateEngine) ContainerRename(ctx context.Context, id string, o dockerclient.ContainerRenameOptions) (dockerclient.ContainerRenameResult, error) {
	return dockerclient.ContainerRenameResult{}, e.mutate(fmt.Sprintf("rename %s -> %s", e.label(id), o.NewName), func() error {
		if e.byRef(o.NewName) != nil {
			return fmt.Errorf("Conflict. The container name %q is already in use", o.NewName)
		}
		e.byRef(id).name = o.NewName
		return nil
	})
}

func (e *stateEngine) ContainerRemove(ctx context.Context, id string, o dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error) {
	return dockerclient.ContainerRemoveResult{}, e.mutate("remove "+e.label(id), func() error {
		if o.RemoveVolumes || o.Force {
			return errors.New("a recreate leftover must be removed without its volumes and without force")
		}
		c := e.byRef(id)
		if c.info.State.Running {
			return errors.New("cannot remove a running container")
		}
		e.removed = append(e.removed, c.info.ID)
		for i, x := range e.containers {
			if x == c {
				e.containers = append(e.containers[:i], e.containers[i+1:]...)
				break
			}
		}
		return nil
	})
}

func (e *stateEngine) revive() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dead = false
	e.killBefore = ""
	e.calls = nil
}

func (e *stateEngine) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func (e *stateEngine) names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.containers {
		out = append(out, c.name)
	}
	sort.Strings(out)
	return out
}

func (e *stateEngine) named(name string) *stateContainer {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.containers {
		if c.name == name {
			return c
		}
	}
	return nil
}

// interrupt runs Recreate against a running (or stopped) jellyfin and
// kills the Engine connection right before op, as a dying hoservad would.
func interrupt(t *testing.T, running bool, op string) *stateEngine {
	t.Helper()
	e := newStateEngine(running)
	e.killBefore = op
	if err := (&EngineClient{cli: e}).Recreate(context.Background(), "jellyfin"); err == nil {
		t.Fatalf("Recreate finished although the Engine was killed before %q", op)
	}
	if !e.dead {
		t.Fatalf("the Engine was never killed: %q was not reached", op)
	}
	e.revive()
	return e
}

// requireOriginalIntact asserts the original container is the only one
// left, under its own name, with the ID, configuration and volumes it
// had, and that nothing was removed but the replacement.
func requireOriginalIntact(t *testing.T, e *stateEngine, running bool) {
	t.Helper()
	if got, want := e.names(), []string{"jellyfin"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("containers after reconciliation = %q, want %q", got, want)
	}
	c := e.named("jellyfin")
	if c.info.ID != oldContainerID {
		t.Fatalf("jellyfin is container %s, want the original %s", c.info.ID, oldContainerID)
	}
	if c.info.State.Running != running {
		t.Fatalf("jellyfin running = %v, want %v", c.info.State.Running, running)
	}
	if c.info.Config.Image != jellyfinRef || c.info.Config.Labels["maintainer"] != "someone" ||
		!reflect.DeepEqual(c.info.HostConfig.Binds, []string{"/mnt/cache/appdata/jellyfin:/config"}) ||
		len(c.info.HostConfig.Mounts) != 1 || c.info.HostConfig.Mounts[0].Source != "jellyfin-cache" || len(c.info.Mounts) != 2 {
		t.Fatalf("the original's configuration or volumes changed: %+v", c.info)
	}
	for _, id := range e.removed {
		if id != newContainerID {
			t.Fatalf("container %s was removed; only the replacement may be", id)
		}
	}
}

func reconcile(t *testing.T, e *stateEngine) ([]Reconciliation, error) {
	t.Helper()
	return (&EngineClient{cli: e}).Reconcile(context.Background())
}

func TestReconcile_NothingLeftBehind(t *testing.T) {
	e := newStateEngine(true)
	got, err := reconcile(t, e)
	if err != nil || len(got) != 0 {
		t.Fatalf("Reconcile = %v, %v, want nothing", got, err)
	}
	if calls := e.recorded(); len(calls) != 0 {
		t.Fatalf("Reconcile changed something on a clean Engine: %q", calls)
	}
}

// A daemon killed at each point of Recreate's swap, for a running and a
// stopped original: the original ends up under its own name, running if
// it was running, and the replacement is gone.
func TestReconcile_RestoresTheOriginalAtEveryInterruptedStep(t *testing.T) {
	cases := []struct {
		name    string
		running bool
		killAt  string
		want    []string
	}{
		{"running, before the original is stopped", true, "stop old", []string{"remove new"}},
		{"running, before the original is renamed aside", true, "rename old -> jellyfin_hoserva-old", []string{"start old", "remove new"}},
		{"running, between the renames", true, "rename new -> jellyfin", []string{
			"rename old -> jellyfin", "start old", "remove new",
		}},
		{"stopped, before the original is renamed aside", false, "rename old -> jellyfin_hoserva-old", []string{"remove new"}},
		{"stopped, between the renames", false, "rename new -> jellyfin", []string{
			"rename old -> jellyfin", "remove new",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := interrupt(t, tc.running, tc.killAt)
			if len(e.names()) != 2 {
				t.Fatalf("the interruption left %q, want the original and a replacement", e.names())
			}
			got, err := reconcile(t, e)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if len(got) != 1 || got[0].Name != "jellyfin" || got[0].Outcome == ReconcileAmbiguous {
				t.Fatalf("Reconcile reported %+v, want one resolved entry for jellyfin", got)
			}
			if calls := e.recorded(); !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("Engine calls:\n got %q\nwant %q", calls, tc.want)
			}
			requireOriginalIntact(t, e, tc.running)
		})
	}
}

// Once the swap has renamed the replacement into place, which of the two
// containers to keep is not something the names decide: nothing is removed
// and the state is reported.
func TestReconcile_LeavesASwapThatWentPastBothRenamesAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		killAt  string
	}{
		{"before the replacement starts", true, "start new"},
		{"before the original is removed", true, "remove old"},
		{"stopped original, before it is removed", false, "remove old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := interrupt(t, tc.running, tc.killAt)
			before := e.names()
			got, err := reconcile(t, e)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if len(got) != 1 || got[0].Outcome != ReconcileAmbiguous {
				t.Fatalf("Reconcile reported %+v, want one ambiguous entry", got)
			}
			if calls := e.recorded(); len(calls) != 0 {
				t.Fatalf("an ambiguous state was changed: %q", calls)
			}
			if !reflect.DeepEqual(e.names(), before) || len(e.removed) != 0 {
				t.Fatalf("containers changed from %q to %q", before, e.names())
			}
		})
	}
}

// A replacement the Engine has started is not a leftover of a swap that
// never got that far: it may hold data the original does not.
func TestReconcile_KeepsAReplacementThatWasStarted(t *testing.T) {
	e := interrupt(t, true, "rename old -> jellyfin_hoserva-old")
	e.named("jellyfin_hoserva-new").info.State = &dockercontainer.State{
		Status: dockercontainer.StateRunning, Running: true, StartedAt: stateEpoch.Add(time.Hour).Format(time.RFC3339Nano),
	}
	got, err := reconcile(t, e)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(got) != 1 || got[0].Outcome != ReconcileAmbiguous {
		t.Fatalf("Reconcile reported %+v, want one ambiguous entry", got)
	}
	if calls := e.recorded(); len(calls) != 0 {
		t.Fatalf("a started replacement was touched: %q", calls)
	}
	if len(e.removed) != 0 || len(e.names()) != 2 {
		t.Fatalf("containers after reconciliation = %q, removed %q", e.names(), e.removed)
	}
}

func TestReconcile_KeepsAReplacementThatStartedAfterTheOriginalWasRestored(t *testing.T) {
	e := interrupt(t, true, "rename new -> jellyfin")
	e.named("jellyfin_hoserva-new").info.State = &dockercontainer.State{
		Status: dockercontainer.StateExited, StartedAt: stateEpoch.Add(time.Hour).Format(time.RFC3339Nano), FinishedAt: stateEpoch.Add(2 * time.Hour).Format(time.RFC3339Nano),
	}
	got, err := reconcile(t, e)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(got) != 1 || got[0].Outcome != ReconcileRestored {
		t.Fatalf("Reconcile reported %+v, want the original restored", got)
	}
	if len(e.removed) != 0 {
		t.Fatalf("a started replacement was removed: %q", e.removed)
	}
	if got, want := e.names(), []string{"jellyfin", "jellyfin_hoserva-new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("containers = %q, want %q", got, want)
	}
	if e.named("jellyfin").info.ID != oldContainerID {
		t.Fatal("the original is not back under its name")
	}
}

// A replacement with no original anywhere is the only copy: it is reported,
// never removed.
func TestReconcile_KeepsALoneReplacement(t *testing.T) {
	e := interrupt(t, true, "rename old -> jellyfin_hoserva-old")
	e.mu.Lock()
	e.containers = e.containers[1:]
	e.mu.Unlock()
	got, err := reconcile(t, e)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(got) != 1 || got[0].Outcome != ReconcileAmbiguous {
		t.Fatalf("Reconcile reported %+v, want one ambiguous entry", got)
	}
	if calls := e.recorded(); len(calls) != 0 || len(e.names()) != 1 {
		t.Fatalf("a lone replacement was touched: %q, containers %q", calls, e.names())
	}
}

// The original alone under the aside name, with nothing to say whether it
// was running, is given its name back and left as the Engine has it.
func TestReconcile_RenamesALoneAsideOriginalBackWithoutStartingIt(t *testing.T) {
	e := interrupt(t, true, "rename new -> jellyfin")
	e.mu.Lock()
	e.containers = e.containers[:1]
	e.mu.Unlock()
	got, err := reconcile(t, e)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(got) != 1 || got[0].Outcome != ReconcileRestored {
		t.Fatalf("Reconcile reported %+v, want the original restored", got)
	}
	if calls, want := e.recorded(), []string{"rename old -> jellyfin"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("Engine calls = %q, want %q", calls, want)
	}
	requireOriginalIntact(t, e, false)
}

// The replacement is removed only once the original is in place: when the
// original cannot be started, both are kept and the error is returned; the
// next pass finishes the job from what the Engine holds.
func TestReconcile_KeepsTheReplacementWhileTheOriginalIsNotBackUp(t *testing.T) {
	e := interrupt(t, true, "rename new -> jellyfin")
	e.fail["start old"] = errors.New("port 8096 is already allocated")
	if _, err := reconcile(t, e); err == nil || !strings.Contains(err.Error(), "port 8096") {
		t.Fatalf("Reconcile error = %v, want the start failure", err)
	}
	if len(e.removed) != 0 {
		t.Fatalf("the replacement was removed with the original not started: %q", e.removed)
	}
	if got, want := e.names(), []string{"jellyfin", "jellyfin_hoserva-new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("containers = %q, want %q", got, want)
	}

	delete(e.fail, "start old")
	e.revive()
	got, err := reconcile(t, e)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(got) != 1 || got[0].Outcome == ReconcileAmbiguous {
		t.Fatalf("second Reconcile reported %+v, want the leftover resolved", got)
	}
	if calls, want := e.recorded(), []string{"start old", "remove new"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("second pass Engine calls = %q, want %q", calls, want)
	}
	requireOriginalIntact(t, e, true)
}

func TestReconcile_DoesNotTouchAnythingWhenTheOriginalCannotBeRenamedBack(t *testing.T) {
	e := interrupt(t, true, "rename new -> jellyfin")
	e.fail["rename old -> jellyfin"] = errors.New("engine busy")
	if _, err := reconcile(t, e); err == nil || !strings.Contains(err.Error(), "engine busy") {
		t.Fatalf("Reconcile error = %v, want the rename failure", err)
	}
	if calls, want := e.recorded(), []string{"rename old -> jellyfin"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("Engine calls = %q, want only the failed rename", calls)
	}
	if len(e.removed) != 0 || len(e.names()) != 2 {
		t.Fatalf("containers = %q, removed %q", e.names(), e.removed)
	}
}

func TestReconcile_RemovalFailureIsReturnedAndTheOriginalStaysInPlace(t *testing.T) {
	e := interrupt(t, true, "stop old")
	e.fail["remove new"] = errors.New("engine busy")
	if _, err := reconcile(t, e); err == nil || !strings.Contains(err.Error(), "engine busy") {
		t.Fatalf("Reconcile error = %v, want the removal failure", err)
	}
	if e.named("jellyfin").info.ID != oldContainerID {
		t.Fatal("the original is not in place")
	}
}

func TestEngineClient_StartedAtReadsTheEnginesLastStart(t *testing.T) {
	e := newStateEngine(true)
	want, err := time.Parse(time.RFC3339Nano, e.containers[0].info.State.StartedAt)
	if err != nil {
		t.Fatal(err)
	}
	c := &EngineClient{cli: e}
	got, err := c.StartedAt(context.Background(), "jellyfin")
	if err != nil || !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("StartedAt = %v, %v, want %v in UTC", got, err, want)
	}

	e.containers[0].info.State.StartedAt = neverStamp
	if got, err := c.StartedAt(context.Background(), "jellyfin"); err != nil || !got.IsZero() {
		t.Fatalf("StartedAt of a container never started = %v, %v, want the zero time", got, err)
	}
}

// An error is never a time: a start time that cannot be read is no start
// time, so a caller cannot take the container for one that never ran.
func TestEngineClient_StartedAtFailsWhenTheEngineGivesNoTime(t *testing.T) {
	c := &EngineClient{cli: newStateEngine(true)}
	if _, err := c.StartedAt(context.Background(), "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("StartedAt of an unknown container = %v, want ErrNotFound", err)
	}
	for name, stamp := range map[string]string{"empty": "", "garbled": "last tuesday"} {
		t.Run(name, func(t *testing.T) {
			e := newStateEngine(true)
			e.containers[0].info.State.StartedAt = stamp
			if got, err := (&EngineClient{cli: e}).StartedAt(context.Background(), "jellyfin"); err == nil {
				t.Fatalf("StartedAt = %v, nil, want an error", got)
			}
		})
	}
}

func TestEngineClient_CreatedAtReadsWhenTheEngineCreatedTheContainer(t *testing.T) {
	e := newStateEngine(true)
	want, err := time.Parse(time.RFC3339Nano, e.containers[0].info.Created)
	if err != nil {
		t.Fatal(err)
	}
	got, err := (&EngineClient{cli: e}).CreatedAt(context.Background(), "jellyfin")
	if err != nil || !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("CreatedAt = %v, %v, want %v in UTC", got, err, want)
	}
}

// An error is never a time: a creation time that cannot be read, or the zero
// time the Engine gives for none, is not one a caller may compare.
func TestEngineClient_CreatedAtFailsWhenTheEngineGivesNoTime(t *testing.T) {
	c := &EngineClient{cli: newStateEngine(true)}
	if _, err := c.CreatedAt(context.Background(), "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreatedAt of an unknown container = %v, want ErrNotFound", err)
	}
	for name, stamp := range map[string]string{"empty": "", "garbled": "last tuesday", "zero": neverStamp} {
		t.Run(name, func(t *testing.T) {
			e := newStateEngine(true)
			e.containers[0].info.Created = stamp
			if got, err := (&EngineClient{cli: e}).CreatedAt(context.Background(), "jellyfin"); err == nil {
				t.Fatalf("CreatedAt = %v, nil, want an error", got)
			}
		})
	}
}
