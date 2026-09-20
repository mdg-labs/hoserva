package disk

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
)

// UnitMounter brings a physical disk's MountUnit up and down. Production
// uses SystemdMounter (config.Generator already wrote the unit file; this
// asks systemd to start it). The loop-device lab has no init system
// (doc 06 §3), so lab tests use DirectMounter, which mounts by
// filesystem UUID with the same What= the unit would have used (Q21).
type UnitMounter interface {
	Mount(ctx context.Context, unit MountUnit) error
	Unmount(ctx context.Context, unit MountUnit) error
}

// DirectMounter mounts unit at unit.Where by filesystem UUID, as an
// argv, never a shell. It is the lab/test path: no systemd, same UUID
// bind MountUnit.Render emits.
type DirectMounter struct {
	Runner Runner
}

// Mount creates unit.Where if needed and mounts UUID=unit.UUID there.
// A disk already mounted at unit.Where by that same UUID is success, so
// a create-array retry after a partial apply does not fail as a second
// format would.
func (m DirectMounter) Mount(ctx context.Context, unit MountUnit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(unit.Where, 0o755); err != nil {
		return fmt.Errorf("disk: creating mountpoint %s: %w", unit.Where, err)
	}
	if _, err := m.Runner.Run(ctx, "mount", "-t", string(unit.Filesystem), "-o", "defaults,nofail", "-U", unit.UUID, unit.Where); err != nil {
		out, findErr := m.Runner.Run(ctx, "findmnt", "-n", "-o", "UUID", unit.Where)
		if findErr == nil && strings.TrimSpace(string(out)) == unit.UUID {
			return nil
		}
		return fmt.Errorf("disk: mounting %s at %s: %w", unit.UUID, unit.Where, err)
	}
	return nil
}

// Unmount unmounts unit.Where as an argv. A path that is already not a
// mountpoint is success, so eject after a partial unmount does not fail.
func (m DirectMounter) Unmount(ctx context.Context, unit MountUnit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := m.Runner.Run(ctx, "umount", unit.Where); err != nil {
		mounted, checkErr := isMountpoint(unit.Where)
		if checkErr == nil && !mounted {
			return nil
		}
		return fmt.Errorf("disk: unmounting %s: %w", unit.Where, err)
	}
	return nil
}

// SystemdMounter starts the systemd .mount unit Generator wrote for
// unit (doc 02 §4). daemon-reload picks up the newly written file
// before start.
type SystemdMounter struct {
	Runner Runner
}

// Mount reloads systemd units and starts unit's mount unit.
func (m SystemdMounter) Mount(ctx context.Context, unit MountUnit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := m.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("disk: daemon-reload before mounting %s: %w", unit.Where, err)
	}
	name := UnitFileName(unit.Where)
	if _, err := m.Runner.Run(ctx, "systemctl", "start", name); err != nil {
		return fmt.Errorf("disk: starting mount unit for %s: %w", unit.Where, err)
	}
	return nil
}

// Unmount stops unit's mount unit.
func (m SystemdMounter) Unmount(ctx context.Context, unit MountUnit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name := UnitFileName(unit.Where)
	if _, err := m.Runner.Run(ctx, "systemctl", "stop", name); err != nil {
		return fmt.Errorf("disk: stopping mount unit for %s: %w", unit.Where, err)
	}
	return nil
}

// FakeMounter records every Mount call so a test can assert a failed
// FormatPlan never mounted anything, and a successful one mounted only
// the plan's own UUID-backed units.
type FakeMounter struct {
	mu         sync.Mutex
	Mounts     []MountUnit
	Unmounts   []MountUnit
	Err        error
	UnmountErr error
}

// NewFakeMounter returns a FakeMounter that records mounts and returns
// nil unless Err is set.
func NewFakeMounter() *FakeMounter {
	return &FakeMounter{}
}

// Mount records unit, or returns Err if the test scripted a failure.
func (f *FakeMounter) Mount(ctx context.Context, unit MountUnit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.Mounts = append(f.Mounts, unit)
	return nil
}

// Unmount records unit, or returns UnmountErr if the test scripted a failure.
func (f *FakeMounter) Unmount(ctx context.Context, unit MountUnit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.UnmountErr != nil {
		return f.UnmountErr
	}
	f.Unmounts = append(f.Unmounts, unit)
	return nil
}

var (
	_ UnitMounter = DirectMounter{}
	_ UnitMounter = SystemdMounter{}
	_ UnitMounter = (*FakeMounter)(nil)
)
