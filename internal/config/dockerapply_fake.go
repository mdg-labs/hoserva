package config

import (
	"context"
	"os"
	"sync"
)

// FakeDirMaker is a scriptable DirMaker (doc 06 §2): it records every path
// ApplyDockerDataRoot asked to create instead of touching a real
// filesystem — the data-root move targets an absolute cache path outside
// Generator.Root, so a test can never rely on Root's own temp-directory
// confinement for it the way every other config test does (CLAUDE.md: no
// real host path in a test).
type FakeDirMaker struct {
	mu      sync.Mutex
	created []string
	err     error
}

// SetErr scripts every future MkdirAll call to fail with err.
func (f *FakeDirMaker) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *FakeDirMaker) MkdirAll(path string, perm os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.created = append(f.created, path)
	return nil
}

// Created returns every path MkdirAll was asked to create, in call order.
func (f *FakeDirMaker) Created() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.created))
	copy(out, f.created)
	return out
}

var _ DirMaker = (*FakeDirMaker)(nil)

// FakeServiceRestarter is a scriptable ServiceRestarter (doc 06 §2): a
// test scripts whether the service is currently active and whether
// Stop/Start succeed, without a real systemd.
type FakeServiceRestarter struct {
	mu        sync.Mutex
	active    bool
	activeErr error
	stopErr   error
	startErr  error
	stopped   bool
	started   bool
}

func (f *FakeServiceRestarter) SetActive(active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = active
}

func (f *FakeServiceRestarter) SetActiveErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activeErr = err
}

func (f *FakeServiceRestarter) SetStopErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopErr = err
}

func (f *FakeServiceRestarter) SetStartErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startErr = err
}

func (f *FakeServiceRestarter) Active(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activeErr != nil {
		return false, f.activeErr
	}
	return f.active, nil
}

func (f *FakeServiceRestarter) Stop(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = true
	return nil
}

func (f *FakeServiceRestarter) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	return nil
}

// Stopped reports whether Stop was called and succeeded.
func (f *FakeServiceRestarter) Stopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// Started reports whether Start was called and succeeded.
func (f *FakeServiceRestarter) Started() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started
}

var _ ServiceRestarter = (*FakeServiceRestarter)(nil)
