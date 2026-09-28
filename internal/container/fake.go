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
}

// NewFakeProvider returns a FakeProvider that reports a recent, reachable
// Engine and no containers or images until scripted otherwise.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{version: EngineVersion{Version: "27.3.1", APIVersion: "1.47", MinAPIVersion: "1.24"}}
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
	copy(out, f.containers)
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
			return c, nil
		}
	}
	return Container{}, ErrNotFound
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
