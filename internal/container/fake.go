package container

import (
	"context"
	"strings"
	"sync"
)

// FakeProvider is a scriptable Provider (CLAUDE.md, doc 06 §2): a test
// sets the Engine version, containers and images it wants to see, or
// scripts unavailability, without a real Docker daemon.
type FakeProvider struct {
	mu         sync.Mutex
	version    EngineVersion
	versionErr error
	containers []Container
	listErr    error
	images     []Image
	imagesErr  error

	pulls          map[string]string
	failures       map[string]error
	reconciliation []Reconciliation
	calls          []FakeCall
	starts         []FakeStart
	logs           map[string]string
	stats          map[string]Stats
	watchers       map[chan StateChange]struct{}
}

// NewFakeProvider returns a FakeProvider that reports a recent, reachable
// Engine and no containers or images until scripted otherwise.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{version: EngineVersion{Version: "29.8.1", APIVersion: "1.54", MinAPIVersion: "1.40"}}
}

// SetVersion scripts the EngineVersion Version returns.
func (f *FakeProvider) SetVersion(v EngineVersion) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version = v
}

// SetUnavailable scripts every method to return ErrUnavailable, as if the
// Docker Engine were not installed or not running (doc 04 §3). A nil err
// defaults to ErrUnavailable itself.
func (f *FakeProvider) SetUnavailable(err error) {
	if err == nil {
		err = ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versionErr = err
	f.listErr = err
	f.imagesErr = err
}

// AddContainer adds c to what List and Inspect return.
func (f *FakeProvider) AddContainer(c Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers = append(f.containers, c)
}

// RemoveContainer drops the container with the ID from what List and Inspect
// return, as if the Engine had deleted it.
func (f *FakeProvider) RemoveContainer(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.containers[:0:0]
	for _, c := range f.containers {
		if c.ID != id {
			kept = append(kept, c)
		}
	}
	f.containers = kept
}

// AddImage adds i to what Images returns.
func (f *FakeProvider) AddImage(i Image) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images = append(f.images, i)
}

func (f *FakeProvider) Version(ctx context.Context) (EngineVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.versionErr != nil {
		return EngineVersion{}, f.versionErr
	}
	return f.version, nil
}

func (f *FakeProvider) List(ctx context.Context) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]Container, len(f.containers))
	for i, c := range f.containers {
		out[i] = f.listedLocked(c)
	}
	return out, nil
}

func (f *FakeProvider) Inspect(ctx context.Context, id string) (Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return Container{}, f.listErr
	}
	for _, c := range f.containers {
		if c.ID == id || c.Name == id {
			return f.listedLocked(c), nil
		}
	}
	return Container{}, ErrNotFound
}

// listedLocked is the container as the Engine's listing reports it: a
// container whose image reference now points at a different image than the
// one it runs is listed under that image's ID, "sha256:<hex>", which reads as
// repository "sha256" and tag "<hex>". What the container was created with
// is what AddContainer was given; ConfiguredImage returns it.
func (f *FakeProvider) listedLocked(c Container) Container {
	if c.Pinned {
		return c
	}
	ref := containerRef(c)
	for _, img := range f.images {
		if img.ID == c.ImageID {
			continue
		}
		for _, t := range img.RepoTags {
			if t == ref {
				c.Image, c.Tag = "sha256", strings.TrimPrefix(c.ImageID, "sha256:")
				return c
			}
		}
	}
	return c
}

func (f *FakeProvider) ConfiguredImage(ctx context.Context, id string) (ConfiguredImage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return ConfiguredImage{}, f.listErr
	}
	for _, c := range f.containers {
		if c.ID == id || c.Name == id {
			if c.Pinned {
				return ConfiguredImage{Ref: c.Image + "@" + c.ImageID, Pinned: true}, nil
			}
			return ConfiguredImage{Ref: containerRef(c)}, nil
		}
	}
	return ConfiguredImage{}, ErrNotFound
}

func (f *FakeProvider) Images(ctx context.Context) ([]Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.imagesErr != nil {
		return nil, f.imagesErr
	}
	out := make([]Image, len(f.images))
	copy(out, f.images)
	return out, nil
}

var _ Provider = (*FakeProvider)(nil)

// RunCall records one call made through a FakeRunner.
type RunCall struct {
	Name string
	Args []string
}

// FakeRunner is a scriptable Runner (CLAUDE.md, doc 06 §2): the same
// shape internal/disk.FakeRunner and internal/parity's own fakes use, for
// ComposeVersion's `docker compose …` subprocess.
type FakeRunner struct {
	mu      sync.Mutex
	outputs map[string][]byte
	errs    map[string]error
	calls   []RunCall
}

// NewFakeRunner returns a FakeRunner with nothing scripted.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{outputs: make(map[string][]byte), errs: make(map[string]error)}
}

func runnerKey(name string, args []string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

// Script sets the output and error a future call with this exact argv
// returns.
func (f *FakeRunner) Script(name string, args []string, output []byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := runnerKey(name, args)
	f.outputs[key] = output
	f.errs[key] = err
}

// Run implements Runner by returning whatever was scripted for this exact
// argv, recording the call regardless.
func (f *FakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, RunCall{Name: name, Args: append([]string(nil), args...)})
	key := runnerKey(name, args)
	return f.outputs[key], f.errs[key]
}

// Calls returns every call made so far, in call order.
func (f *FakeRunner) Calls() []RunCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RunCall, len(f.calls))
	copy(out, f.calls)
	return out
}

var _ Runner = (*FakeRunner)(nil)
