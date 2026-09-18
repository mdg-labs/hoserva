package disk

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrBootDevice is returned when an operation that can destroy data is
// attempted against the boot device — it is always identified and
// always excluded (doc 02 §4).
var ErrBootDevice = errors.New("disk: refusing to operate on the boot device")

// LinuxProvider is the real Provider (doc 01 §4): it enumerates block
// devices from sysfs, resolves identity from /dev/disk/by-id (Q21),
// polls health with `smartctl -j -n standby`, tracks SMART trends, and
// records every observed spin-state transition (Q32) — all without ever
// waking a disk that was not already awake.
type LinuxProvider struct {
	Lister *Lister
	Exec   Runner
	Events *SpinEventLog
	Trends *TrendTracker
	Now    Clock
}

// NewLinuxProvider returns a LinuxProvider reading the real system paths
// and exec'ing real binaries.
func NewLinuxProvider() *LinuxProvider {
	return &LinuxProvider{
		Lister: NewLister(),
		Exec:   CommandRunner{},
		Events: NewSpinEventLog(),
		Trends: NewTrendTracker(),
		Now:    time.Now,
	}
}

// List enumerates every disk. It reads only sysfs, /dev/disk/by-id and a
// mounts listing, so it never risks waking a disk.
func (p *LinuxProvider) List(ctx context.Context) ([]Disk, error) {
	return p.Lister.List(ctx)
}

// SMART polls dev's health via `smartctl -j`, adding `-n standby` unless
// mode is SMARTPollForce. It records the observed spin state in Events
// without any extra query — the state comes from the same smartctl
// invocation that respected standby — and, for a real (non-Skipped)
// reading, updates Trends and sets the report's overall error trend.
func (p *LinuxProvider) SMART(ctx context.Context, dev string, mode SMARTPollMode) (SMARTReport, error) {
	if err := ctx.Err(); err != nil {
		return SMARTReport{}, err
	}

	args := []string{"-j"}
	if mode == SMARTPollRespectStandby {
		args = append(args, "-n", "standby")
	}
	args = append(args, "-a", dev)

	out, runErr := p.Exec.Run(ctx, "smartctl", args...)
	report, parseErr := ParseSMARTJSON(out)
	if parseErr != nil {
		if runErr != nil {
			return SMARTReport{}, fmt.Errorf("smartctl %s: %w", dev, runErr)
		}
		return SMARTReport{}, fmt.Errorf("smartctl %s: %w", dev, parseErr)
	}

	p.Events.Observe(dev, report.SpinState, p.Now())
	if !report.Skipped {
		report.Trend = p.Trends.Update(dev, report).OverallErrorTrend()
	}
	return report, nil
}

// Spindown puts dev into standby via `hdparm -y`, refusing the boot
// device, and records the resulting transition.
func (p *LinuxProvider) Spindown(ctx context.Context, dev string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.refuseBootDevice(ctx, dev); err != nil {
		return err
	}

	if _, err := p.Exec.Run(ctx, "hdparm", "-y", dev); err != nil {
		return fmt.Errorf("hdparm -y %s: %w", dev, err)
	}
	p.Events.Observe(dev, Standby, p.Now())
	return nil
}

// Format refuses the boot device, then execs the filesystem tool's own
// mkfs command for fs as an argv — never a shell (CLAUDE.md) — to build
// a fresh filesystem on dev (doc 02 §4 "Adding a disk" step 3, doc 02
// §5). Callers that must never touch a disk outside an explicit
// array-setup plan go through FormatAssigned, not this method directly.
func (p *LinuxProvider) Format(ctx context.Context, dev string, fs FilesystemType) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.refuseBootDevice(ctx, dev); err != nil {
		return err
	}
	argv, err := formatCommand(dev, fs)
	if err != nil {
		return err
	}
	if _, err := p.Exec.Run(ctx, argv[0], argv[1:]...); err != nil {
		return fmt.Errorf("format %s: %w", dev, err)
	}
	return nil
}

// refuseBootDevice compares whole-disk paths: List's disks are always
// whole disks (enumerate.go skips partitions), but dev here can be a
// partition — WholeDiskDevice(dev) is what actually identifies which disk
// it belongs to, so a boot disk's own partition (e.g. "/dev/sda2" when
// "/dev/sda" is recorded as boot) must be refused too, not just the exact
// whole-disk path.
func (p *LinuxProvider) refuseBootDevice(ctx context.Context, dev string) error {
	disks, err := p.List(ctx)
	if err != nil {
		return err
	}
	whole := WholeDiskDevice(dev)
	for _, d := range disks {
		if d.Boot && d.Device == whole {
			return fmt.Errorf("%s: %w", dev, ErrBootDevice)
		}
	}
	return nil
}

var _ Provider = (*LinuxProvider)(nil)
