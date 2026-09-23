package job

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// FakeMountTable is MountTable's scriptable fake (CLAUDE.md: every
// system-touching subsystem has one). It keeps a stack of filesystem
// UUIDs per path, so a mount over a mounted path stacks the way the
// kernel's does, and one unmount pops one layer.
type FakeMountTable struct {
	mu     sync.Mutex
	stacks map[string][]string
	// UnmountErr makes every unmount of a path fail and leave it mounted.
	UnmountErr map[string]error
	// MountErr makes a mount at a path fail.
	MountErr map[string]error
	// ReportUUID makes MountedUUID report this UUID for a path, whatever
	// is mounted there.
	ReportUUID map[string]string
	// TableErr makes every mount-table read fail.
	TableErr error
	// OnUnmount, when set, runs before every unmount attempt.
	OnUnmount func(path string)
	ops       []string
}

// NewFakeMountTable returns an empty mount table.
func NewFakeMountTable() *FakeMountTable {
	return &FakeMountTable{
		stacks:     make(map[string][]string),
		UnmountErr: make(map[string]error),
		MountErr:   make(map[string]error),
		ReportUUID: make(map[string]string),
	}
}

// Preload mounts uuid at path without recording an operation — what boot
// or an earlier process left behind.
func (f *FakeMountTable) Preload(path, uuid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := filepath.Clean(path)
	f.stacks[p] = append(f.stacks[p], uuid)
}

// Top returns the UUID visible at path, and whether path is mounted.
func (f *FakeMountTable) Top(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.stacks[filepath.Clean(path)]
	if len(s) == 0 {
		return "", false
	}
	return s[len(s)-1], true
}

// MountedPaths returns every mounted path.
func (f *FakeMountTable) MountedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p, s := range f.stacks {
		if len(s) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// Ops returns every mount ("mount <path> <uuid>") and unmount
// ("umount <path>") attempt, in order.
func (f *FakeMountTable) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// IsMounted reports whether path has anything mounted.
func (f *FakeMountTable) IsMounted(ctx context.Context, path string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.TableErr != nil {
		return false, f.TableErr
	}
	return len(f.stacks[filepath.Clean(path)]) > 0, nil
}

// UnmountOnce pops one layer at path, or fails as scripted. Unmounting a
// path with nothing mounted fails, as umount does.
func (f *FakeMountTable) UnmountOnce(ctx context.Context, path string) error {
	f.mu.Lock()
	hook := f.OnUnmount
	f.mu.Unlock()
	if hook != nil {
		hook(path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := filepath.Clean(path)
	f.ops = append(f.ops, "umount "+p)
	if err := f.UnmountErr[p]; err != nil {
		return err
	}
	s := f.stacks[p]
	if len(s) == 0 {
		return fmt.Errorf("exit status 32: umount: %s: not mounted", p)
	}
	f.stacks[p] = s[:len(s)-1]
	return nil
}

// Mount pushes unit.UUID at unit.Where, or fails as scripted.
func (f *FakeMountTable) Mount(ctx context.Context, unit disk.MountUnit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := filepath.Clean(unit.Where)
	f.ops = append(f.ops, "mount "+p+" "+unit.UUID)
	if err := f.MountErr[p]; err != nil {
		return err
	}
	f.stacks[p] = append(f.stacks[p], unit.UUID)
	return nil
}

// MountedUUID reports the UUID visible at path.
func (f *FakeMountTable) MountedUUID(ctx context.Context, path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.TableErr != nil {
		return "", f.TableErr
	}
	p := filepath.Clean(path)
	s := f.stacks[p]
	if len(s) == 0 {
		return "", errors.New("exit status 1: findmnt: " + p + " is not a mountpoint")
	}
	if u, ok := f.ReportUUID[p]; ok {
		return u, nil
	}
	return s[len(s)-1], nil
}

var _ MountTable = (*FakeMountTable)(nil)
