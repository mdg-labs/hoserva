package disk

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
)

// UnitMounter brings a physical disk's MountUnit up and down. Array slots
// use SystemdMounter in production (config.Generator already wrote the unit
// file; this asks systemd to start it). DirectMounter mounts by filesystem
// UUID with the same What= the unit would have used (Q21) and starts no
// unit. Production uses it for the disk-upgrade jobs and external disks
// (see DirectMounter), and the loop-device lab, which has no init system
// (doc 06 §3), uses it in place of SystemdMounter.
type UnitMounter interface {
	Mount(ctx context.Context, unit MountUnit) error
	Unmount(ctx context.Context, unit MountUnit) error
}

// DirectMounter mounts unit at unit.Where by filesystem UUID, as an
// argv, never a shell: no systemd, same UUID bind MountUnit.Render emits.
// It is a production mounter as well as the lab's: hoservad uses it for the
// new parity disk's initial mount in a parity-disk upgrade
// (DiskUpgradeParityDeps.UpgradeMounter), for external disks
// (api.Handler.DiskMounter, whose nil default it is), and, through
// KernelMounts.Mount, for the data-disk upgrade's mounts. Its checks, such
// as the post-mount UUID confirmation, therefore guard real disks.
type DirectMounter struct {
	Runner Runner
}

// Mount creates unit.Where if needed and mounts the unit's filesystem there:
// by the device unit.What names when it is set (a read-only adoption mount
// bound to the disk's own identity), else UUID=unit.UUID. A disk already
// mounted at unit.Where by that same UUID is success, so a create-array retry
// after a partial apply does not fail as a second format would. Success is
// only returned once the mount table shows unit.UUID at unit.Where, whatever
// mount's exit status was; a unit with What also shows that exact device as
// the mount's source, which a UUID alone could not (a byte copy of the
// filesystem on another disk has the same one), and a read-only unit shows
// the mount read-only.
func (m DirectMounter) Mount(ctx context.Context, unit MountUnit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	opts := "defaults,nofail"
	if unit.ReadOnly {
		ro, ok := ReadOnlyOptions(unit.Filesystem)
		if !ok {
			return fmt.Errorf("disk: no read-only mount options are known for filesystem type %q", unit.Filesystem)
		}
		// nofail would make mount exit 0 for a device that is not there.
		opts = ro
	}
	if err := os.MkdirAll(unit.Where, 0o755); err != nil {
		return fmt.Errorf("disk: creating mountpoint %s: %w", unit.Where, err)
	}
	argv := []string{"-t", string(unit.Filesystem), "-o", opts}
	if unit.What != "" {
		argv = append(argv, unit.What, unit.Where)
	} else {
		argv = append(argv, "-U", unit.UUID, unit.Where)
	}
	if _, err := m.Runner.Run(ctx, "mount", argv...); err != nil {
		out, findErr := m.Runner.Run(ctx, "findmnt", "-n", "-o", "UUID", unit.Where)
		if findErr == nil && strings.TrimSpace(string(out)) == unit.UUID && m.confirmUnit(ctx, unit) == nil {
			return nil
		}
		return fmt.Errorf("disk: mounting %s at %s: %w", unit.UUID, unit.Where, err)
	}
	// nofail makes mount(8) exit 0 when no device carries the UUID, so the
	// exit status alone does not say the filesystem is there.
	if err := ConfirmMountedUUID(ctx, m.Runner, unit.Where, unit.UUID); err != nil {
		return fmt.Errorf("disk: mounting %s at %s exited 0 but the filesystem is not mounted there: %w", unit.UUID, unit.Where, err)
	}
	if err := m.confirmUnit(ctx, unit); err != nil {
		return fmt.Errorf("disk: mounting %s at %s: %w", unit.UUID, unit.Where, err)
	}
	return nil
}

// confirmUnit shows what a unit's own options promise: the mount's source is
// the device unit.What names, and a read-only unit is mounted read-only.
func (m DirectMounter) confirmUnit(ctx context.Context, unit MountUnit) error {
	if unit.What != "" {
		if err := ConfirmMountedSource(ctx, m.Runner, unit.Where, unit.What); err != nil {
			return err
		}
	}
	if unit.ReadOnly {
		return ConfirmMountedReadOnly(ctx, m.Runner, unit.Where)
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
