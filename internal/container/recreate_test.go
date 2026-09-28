package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	dockermount "github.com/docker/docker/api/types/mount"
	dockernetwork "github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// scriptedEngine is an Engine holding one container, recording every call
// it receives and failing the ones a test names. Everything it does not
// override panics (nil embedded interface), so a test also proves
// Recreate makes no call it was not meant to.
type scriptedEngine struct {
	engineAPI

	mu      sync.Mutex
	calls   []string
	fail    map[string]error
	pullMsg string
	info    dockercontainer.InspectResponse
	newID   string
}

const (
	oldContainerID = "0123456789abcdef0123456789abcdef"
	newContainerID = "fedcba9876543210fedcba9876543210"
)

func newScriptedEngine(running bool) *scriptedEngine {
	state := "exited"
	if running {
		state = "running"
	}
	return &scriptedEngine{
		fail:  map[string]error{},
		newID: newContainerID,
		info: dockercontainer.InspectResponse{
			ContainerJSONBase: &dockercontainer.ContainerJSONBase{
				ID:         oldContainerID,
				Name:       "/jellyfin",
				State:      &dockercontainer.State{Status: dockercontainer.ContainerState(state), Running: running},
				HostConfig: &dockercontainer.HostConfig{Binds: []string{"/mnt/cache/appdata/jellyfin:/config"}},
			},
			Config: &dockercontainer.Config{
				Image:    "lscr.io/linuxserver/jellyfin:10.9.7",
				Hostname: oldContainerID[:12],
				Labels:   map[string]string{"maintainer": "someone"},
			},
			Mounts: []dockercontainer.MountPoint{
				{Type: dockermount.TypeBind, Source: "/mnt/cache/appdata/jellyfin", Destination: "/config", RW: true},
			},
			NetworkSettings: &dockercontainer.NetworkSettings{},
		},
	}
}

// record notes op and returns the failure scripted for failKey.
func (e *scriptedEngine) record(op, failKey string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, op)
	return e.fail[failKey]
}

func (e *scriptedEngine) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func (e *scriptedEngine) ContainerList(ctx context.Context, o dockercontainer.ListOptions) ([]dockercontainer.Summary, error) {
	return []dockercontainer.Summary{{ID: oldContainerID, Names: []string{"/jellyfin"}, State: e.info.State.Status}}, nil
}

func (e *scriptedEngine) ContainerInspect(ctx context.Context, id string) (dockercontainer.InspectResponse, error) {
	return e.info, nil
}

func (e *scriptedEngine) ImagePull(ctx context.Context, ref string, o dockerimage.PullOptions) (io.ReadCloser, error) {
	if err := e.record("pull "+ref, "pull "+ref); err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(e.pullMsg)), nil
}

func (e *scriptedEngine) ContainerCreate(ctx context.Context, c *dockercontainer.Config, h *dockercontainer.HostConfig, n *dockernetwork.NetworkingConfig, p *ocispec.Platform, name string) (dockercontainer.CreateResponse, error) {
	if err := e.record("create "+name, "create"); err != nil {
		return dockercontainer.CreateResponse{}, err
	}
	return dockercontainer.CreateResponse{ID: e.newID}, nil
}

func (e *scriptedEngine) step(op string, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, op+" "+e.label(id))
	return e.fail[op+" "+e.label(id)]
}

func (e *scriptedEngine) label(id string) string {
	switch id {
	case oldContainerID:
		return "old"
	case newContainerID:
		return "new"
	}
	return id
}

func (e *scriptedEngine) ContainerStop(ctx context.Context, id string, o dockercontainer.StopOptions) error {
	return e.step("stop", id)
}

func (e *scriptedEngine) ContainerStart(ctx context.Context, id string, o dockercontainer.StartOptions) error {
	return e.step("start", id)
}

func (e *scriptedEngine) ContainerRename(ctx context.Context, id, name string) error {
	e.mu.Lock()
	e.calls = append(e.calls, fmt.Sprintf("rename %s -> %s", e.label(id), name))
	err := e.fail["rename "+e.label(id)+" -> "+name]
	e.mu.Unlock()
	return err
}

func (e *scriptedEngine) ContainerRemove(ctx context.Context, id string, o dockercontainer.RemoveOptions) error {
	err := e.step("remove", id)
	if o.RemoveVolumes || o.Force {
		return errors.New("Recreate must never remove volumes or force a removal")
	}
	return err
}

func recreate(t *testing.T, e *scriptedEngine) error {
	t.Helper()
	return (&EngineClient{cli: e}).Recreate(context.Background(), "jellyfin")
}

func assertCalls(t *testing.T, e *scriptedEngine, want ...string) {
	t.Helper()
	if got := e.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls:\n got %q\nwant %q", got, want)
	}
}

const jellyfinRef = "lscr.io/linuxserver/jellyfin:10.9.7"

func TestRecreate_Success(t *testing.T) {
	e := newScriptedEngine(true)
	if err := recreate(t, e); err != nil {
		t.Fatalf("Recreate: %v", err)
	}
	assertCalls(t, e,
		"pull "+jellyfinRef,
		"create jellyfin_hoserva-new",
		"stop old",
		"rename old -> jellyfin_hoserva-old",
		"rename new -> jellyfin",
		"start new",
		"remove old",
	)
}

func TestRecreate_StoppedContainerStaysStopped(t *testing.T) {
	e := newScriptedEngine(false)
	if err := recreate(t, e); err != nil {
		t.Fatalf("Recreate: %v", err)
	}
	assertCalls(t, e,
		"pull "+jellyfinRef,
		"create jellyfin_hoserva-new",
		"rename old -> jellyfin_hoserva-old",
		"rename new -> jellyfin",
		"remove old",
	)
}

// The data-loss scenario: the pull fails (registry down, tag gone). The
// original container, its name and its volumes must not have been touched.
func TestRecreate_FailedPullTouchesNothing(t *testing.T) {
	e := newScriptedEngine(true)
	e.fail["pull "+jellyfinRef] = errors.New("manifest unknown")

	if err := recreate(t, e); err == nil {
		t.Fatal("Recreate succeeded although the pull failed")
	}
	assertCalls(t, e, "pull "+jellyfinRef)
}

// The Engine reports a failed pull inside a 200 progress stream.
func TestRecreate_PullErrorInTheProgressStreamTouchesNothing(t *testing.T) {
	e := newScriptedEngine(true)
	e.pullMsg = `{"status":"Pulling from x"}` + "\n" + `{"errorDetail":{"message":"denied"},"error":"pull access denied"}` + "\n"

	err := recreate(t, e)
	if err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("Recreate error = %v, want the pull failure", err)
	}
	assertCalls(t, e, "pull "+jellyfinRef)
}

func TestRecreate_FailedCreateTouchesNothing(t *testing.T) {
	e := newScriptedEngine(true)
	e.fail["create"] = errors.New("invalid mount config")

	if err := recreate(t, e); err == nil {
		t.Fatal("Recreate succeeded although the create failed")
	}
	assertCalls(t, e, "pull "+jellyfinRef, "create jellyfin_hoserva-new")
}

// The replacement fails to start (say, a port is taken): the original must
// get its name back and run again, and the replacement must be removed —
// never the original.
func TestRecreate_FailedStartRestoresTheOriginal(t *testing.T) {
	e := newScriptedEngine(true)
	e.fail["start new"] = errors.New("port is already allocated")

	err := recreate(t, e)
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("Recreate error = %v, want it to say the original was restored", err)
	}
	assertCalls(t, e,
		"pull "+jellyfinRef,
		"create jellyfin_hoserva-new",
		"stop old",
		"rename old -> jellyfin_hoserva-old",
		"rename new -> jellyfin",
		"start new",
		"stop new",
		"rename new -> jellyfin_hoserva-new",
		"rename old -> jellyfin",
		"start old",
		"remove new",
	)
}

func TestRecreate_FailedStopOfTheOriginalStartsItAgain(t *testing.T) {
	e := newScriptedEngine(true)
	e.fail["stop old"] = errors.New("context deadline exceeded")

	if err := recreate(t, e); err == nil {
		t.Fatal("Recreate succeeded")
	}
	assertCalls(t, e,
		"pull "+jellyfinRef,
		"create jellyfin_hoserva-new",
		"stop old",
		"start old",
		"remove new",
	)
}

func TestRecreate_FailedRenameOfTheReplacementRestoresTheOriginal(t *testing.T) {
	e := newScriptedEngine(true)
	e.fail["rename new -> jellyfin"] = errors.New("name conflict")

	if err := recreate(t, e); err == nil {
		t.Fatal("Recreate succeeded")
	}
	assertCalls(t, e,
		"pull "+jellyfinRef,
		"create jellyfin_hoserva-new",
		"stop old",
		"rename old -> jellyfin_hoserva-old",
		"rename new -> jellyfin",
		"rename old -> jellyfin",
		"start old",
		"remove new",
	)
}

// A rollback that cannot complete says so, naming the name the original
// container is left under.
func TestRecreate_IncompleteRollbackIsReported(t *testing.T) {
	e := newScriptedEngine(true)
	e.fail["start new"] = errors.New("port is already allocated")
	e.fail["rename old -> jellyfin"] = errors.New("name in use")

	err := recreate(t, e)
	if err == nil || !strings.Contains(err.Error(), "could not be fully restored") || !strings.Contains(err.Error(), "jellyfin_hoserva-old") {
		t.Fatalf("Recreate error = %v, want an incomplete-rollback error naming jellyfin_hoserva-old", err)
	}
	for _, c := range e.recorded() {
		if c == "remove new" {
			t.Fatalf("the replacement was removed although the original was not restored: %q", e.recorded())
		}
	}
}

// dockerd deletes a --rm container the moment it stops, so its stop would
// destroy the original before the replacement takes its place.
func TestRecreate_AutoRemoveContainerIsRefusedBeforeAnythingChanges(t *testing.T) {
	e := newScriptedEngine(true)
	e.info.HostConfig.AutoRemove = true

	err := recreate(t, e)
	if err == nil || !strings.Contains(err.Error(), "--rm") {
		t.Fatalf("Recreate error = %v, want a refusal naming --rm", err)
	}
	assertCalls(t, e)
}

// The original is gone (nothing to rename or start again): the replacement
// is then the only copy of the container and must survive the rollback.
func TestRecreate_RollbackKeepsTheReplacementWhenTheOriginalIsGone(t *testing.T) {
	e := newScriptedEngine(true)
	gone := errors.New("No such container")
	e.fail["rename old -> jellyfin_hoserva-old"] = gone
	e.fail["start old"] = gone

	err := recreate(t, e)
	if err == nil || !strings.Contains(err.Error(), "could not be fully restored") || !strings.Contains(err.Error(), "jellyfin_hoserva-new") {
		t.Fatalf("Recreate error = %v, want an incomplete-rollback error naming the kept replacement", err)
	}
	for _, c := range e.recorded() {
		if c == "remove new" {
			t.Fatalf("the replacement was removed although the original was not restored: %q", e.recorded())
		}
	}
}

// The replacement already runs; only the leftover original cannot be
// removed. That is reported, and the working replacement is left alone.
func TestRecreate_FailedRemovalOfTheOriginalKeepsTheReplacement(t *testing.T) {
	e := newScriptedEngine(true)
	e.fail["remove old"] = errors.New("device busy")

	err := recreate(t, e)
	if err == nil || !strings.Contains(err.Error(), "jellyfin_hoserva-old") {
		t.Fatalf("Recreate error = %v, want it to name the leftover original", err)
	}
	for _, c := range e.recorded() {
		if c == "stop new" || c == "remove new" {
			t.Fatalf("the working replacement was touched: %q", e.recorded())
		}
	}
}

func TestRecreateSpec_CarriesAnonymousVolumesAndClearsRuntimeValues(t *testing.T) {
	e := newScriptedEngine(true)
	old := e.info
	old.HostConfig.Mounts = []dockermount.Mount{{Type: dockermount.TypeBind, Source: "/srv/x", Target: "/x"}}
	old.Mounts = []dockercontainer.MountPoint{
		{Type: dockermount.TypeBind, Source: "/mnt/cache/appdata/jellyfin", Destination: "/config", RW: true},
		{Type: dockermount.TypeBind, Source: "/srv/x", Destination: "/x", RW: true},
		{Type: dockermount.TypeVolume, Name: "0ff1ce", Source: "/var/lib/docker/volumes/0ff1ce/_data", Destination: "/data", RW: true},
		{Type: dockermount.TypeVolume, Name: "ro-vol", Destination: "/cfg", RW: false},
	}
	old.NetworkSettings = &dockercontainer.NetworkSettings{Networks: map[string]*dockernetwork.EndpointSettings{
		"bridge": {NetworkID: "n1", EndpointID: "e1", IPAddress: "172.17.0.2"},
		"lan":    {Aliases: []string{"jellyfin", oldContainerID[:12]}, IPAMConfig: &dockernetwork.EndpointIPAMConfig{IPv4Address: "10.0.0.5"}, IPAddress: "10.0.0.5"},
		"host":   {},
	}}

	cfg, host, nets := recreateSpec(old)

	if cfg.Hostname != "" {
		t.Fatalf("Hostname = %q, want the old container's generated hostname dropped", cfg.Hostname)
	}
	if cfg.Image != jellyfinRef {
		t.Fatalf("Image = %q", cfg.Image)
	}
	var vols []dockermount.Mount
	for _, m := range host.Mounts {
		if m.Type == dockermount.TypeVolume {
			vols = append(vols, m)
		}
	}
	want := []dockermount.Mount{
		{Type: dockermount.TypeVolume, Source: "0ff1ce", Target: "/data"},
		{Type: dockermount.TypeVolume, Source: "ro-vol", Target: "/cfg", ReadOnly: true},
	}
	if !reflect.DeepEqual(vols, want) {
		t.Fatalf("volume mounts = %+v, want %+v", vols, want)
	}
	if len(host.Mounts) != 3 {
		t.Fatalf("Mounts = %+v, want the original bind mount kept and the two volumes added, no duplicates", host.Mounts)
	}
	if !reflect.DeepEqual(host.Binds, old.HostConfig.Binds) {
		t.Fatalf("Binds = %v, want them unchanged", host.Binds)
	}
	if _, ok := nets.EndpointsConfig["host"]; ok {
		t.Fatal("the host network was given an endpoint")
	}
	lan := nets.EndpointsConfig["lan"]
	if lan == nil || !reflect.DeepEqual(lan.Aliases, []string{"jellyfin"}) || lan.IPAMConfig == nil || lan.IPAddress != "" {
		t.Fatalf("lan endpoint = %+v, want the alias and static address kept and the old short-ID alias and runtime address dropped", lan)
	}
	if b := nets.EndpointsConfig["bridge"]; b == nil || b.EndpointID != "" || b.NetworkID != "" {
		t.Fatalf("bridge endpoint = %+v, want runtime ids dropped", b)
	}
	// The original description must not be modified: it is the rollback
	// path's only record of the container.
	if len(old.HostConfig.Mounts) != 1 {
		t.Fatalf("recreateSpec modified the original host config: %+v", old.HostConfig.Mounts)
	}
}

// A container Hoserva did not install carries none of Hoserva's labels; the
// replacement must carry exactly the original's, nothing added.
func TestRecreateSpec_AddsNoLabels(t *testing.T) {
	e := newScriptedEngine(true)
	cfg, _, _ := recreateSpec(e.info)
	if !reflect.DeepEqual(cfg.Labels, map[string]string{"maintainer": "someone"}) {
		t.Fatalf("Labels = %v, want the original's, unchanged", cfg.Labels)
	}
}
