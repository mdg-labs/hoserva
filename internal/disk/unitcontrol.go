package disk

import (
	"context"
	"fmt"
	"strings"
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

// ServiceUnitController stops and starts a system service through its
// systemd service unit — Samba and NFS's own doc 02 §4 "Samba and NFS
// stop"/"start" steps in the array stop/start sequence, run through the
// same systemctl mechanism MountUnitController uses for disk mounts.
// Its Name/Stop/Start shape matches internal/job.ArrayService
// structurally, with no import of that package needed (the same
// division MountUnitController keeps from internal/job.ArrayMount).
//
// Neither Samba nor NFS is a hard dependency of the `.deb` (#331), so a
// host can legitimately have one installed and not the other. Stop and
// Start each check the unit's LoadState first: `not-found` means the
// package was never installed, so there is nothing to stop or start and
// the step is skipped. Any other state runs the real systemctl
// stop/start, and a query error aborts Stop/Start with that error
// instead of attempting it — a query error must never be read as
// "nothing to do", because that would let the array sequence unmount
// storage a service might still be holding open.
type ServiceUnitController struct {
	// ServiceName is what Stop/Start errors and job.ArraySequence's own
	// wrapping ("job: stopping <name>: …") name — a human label, not the
	// unit file name.
	ServiceName string
	// Unit is the systemd unit systemctl acts on, e.g. "smbd.service".
	Unit   string
	Runner Runner
}

// Name identifies the service for logging and error messages.
func (c ServiceUnitController) Name() string { return c.ServiceName }

// installed reports whether c.Unit is known to systemd, by its
// LoadState. Only an exact "not-found" is treated as "nothing to do" —
// every other state (loaded, masked, an unrecognized value) means the
// unit genuinely exists, so Stop/Start acts on it; a query error is
// returned rather than treated as either answer, so it aborts Stop/Start
// with that error instead of silently skipping the step.
func (c ServiceUnitController) installed(ctx context.Context) (bool, error) {
	out, err := c.Runner.Run(ctx, "systemctl", "show", "--property=LoadState", "--value", c.Unit)
	if err != nil {
		return false, fmt.Errorf("checking %s unit state: %w", c.ServiceName, err)
	}
	return strings.TrimSpace(string(out)) != "not-found", nil
}

// Stop runs `systemctl stop <unit>`, unless the unit is not installed.
func (c ServiceUnitController) Stop(ctx context.Context) error {
	present, err := c.installed(ctx)
	if err != nil {
		return fmt.Errorf("disk: %w", err)
	}
	if !present {
		return nil
	}
	if _, err := c.Runner.Run(ctx, "systemctl", "stop", c.Unit); err != nil {
		return fmt.Errorf("disk: stopping %s: %w", c.ServiceName, err)
	}
	return nil
}

// Start runs `systemctl start <unit>`, unless the unit is not installed.
func (c ServiceUnitController) Start(ctx context.Context) error {
	present, err := c.installed(ctx)
	if err != nil {
		return fmt.Errorf("disk: %w", err)
	}
	if !present {
		return nil
	}
	if _, err := c.Runner.Run(ctx, "systemctl", "start", c.Unit); err != nil {
		return fmt.Errorf("disk: starting %s: %w", c.ServiceName, err)
	}
	return nil
}
