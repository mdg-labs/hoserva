package disk

import (
	"context"
	"fmt"
)

// MountUnitController brings a physical disk's own systemd .mount unit
// (MountUnit) up and down through systemctl — the doc 02 §4 stop/start
// sequence's own "disks unmount" and "disks mount" steps. Unlike pool's
// Mounter, which execs mergerfs directly (the loop-device lab has no
// init system, doc 06 §3, doc 08 §6), a physical disk's mount is always
// systemd-managed in production: MountUnit.Render's content is what
// config.Generator (D4) writes to
// /etc/systemd/system/<UnitFileName>, and this type is what asks systemd
// to act on it. Its Where/Mount/Unmount shape matches the doc 02 §4
// sequence's generic mount step (internal/job.ArrayMount) structurally,
// with no import of that package needed.
type MountUnitController struct {
	Unit   MountUnit
	Runner Runner
}

// Where is the disk's own mountpoint, for logging and error messages.
func (c MountUnitController) Where() string { return c.Unit.Where }

// Mount runs `systemctl start <unit>`.
func (c MountUnitController) Mount(ctx context.Context) error {
	if _, err := c.Runner.Run(ctx, "systemctl", "start", UnitFileName(c.Unit.Where)); err != nil {
		return fmt.Errorf("disk: starting mount unit for %s: %w", c.Unit.Where, err)
	}
	return nil
}

// Unmount runs `systemctl stop <unit>`.
func (c MountUnitController) Unmount(ctx context.Context) error {
	if _, err := c.Runner.Run(ctx, "systemctl", "stop", UnitFileName(c.Unit.Where)); err != nil {
		return fmt.Errorf("disk: stopping mount unit for %s: %w", c.Unit.Where, err)
	}
	return nil
}
