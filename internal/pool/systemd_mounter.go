package pool

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// SystemdMounter brings a Mount up and down through its generated
// systemd .mount unit (config.WritePoolMounts / WriteCatchAllMount).
// Production hoservad uses this so the mergerfs process lives in the
// mount unit's own cgroup, not hoserva.service's — a restart or crash of
// hoservad then leaves the pool serving (#335). The loop-device lab has
// no init system (doc 06 §3), so lab tests keep using Mounter, which
// execs mergerfs directly.
//
// Mount/Unmount share Mounter's already-own and busy-retry behaviour:
// an already-live mount with mnt.FSName is adopted via applyRuntime
// (#268), and a busy unmount is retried within unmountRetryWindow.
type SystemdMounter struct {
	Runner disk.Runner

	Now          disk.Clock
	Sleep        disk.Sleeper
	IsMountpoint func(where string) (bool, error)
	SetXattr     func(path, attr string, value []byte) error
}

func (m SystemdMounter) asDirect() Mounter {
	return Mounter(m)
}

// Mount reloads systemd units and starts mnt's .mount unit. A path that
// is already a mount point with mnt.FSName is adopted in place (#268,
// #335): applyRuntime updates branches/policy without stacking a second
// mergerfs, and systemd is not asked to start an already-active unit.
func (m SystemdMounter) Mount(ctx context.Context, mnt Mount) error {
	if err := os.MkdirAll(mnt.Where, 0o755); err != nil {
		return fmt.Errorf("pool: creating mount point %s: %w", mnt.Where, err)
	}

	direct := m.asDirect()
	mounted, err := direct.isMountpoint()(mnt.Where)
	if err != nil {
		return fmt.Errorf("pool: checking whether %s is already mounted: %w", mnt.Where, err)
	}
	if mounted {
		out, err := m.Runner.Run(ctx, "findmnt", "-n", "-o", "SOURCE", mnt.Where)
		if err != nil {
			return fmt.Errorf("pool: %s is already mounted, and its filesystem could not be identified: %w", mnt.Where, err)
		}
		if strings.TrimSpace(string(out)) == mnt.FSName {
			return direct.applyRuntime(mnt)
		}
		return fmt.Errorf("pool: %s is already mounted by something other than %s", mnt.Where, mnt.FSName)
	}

	if _, err := m.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("pool: daemon-reload before mounting %s: %w", mnt.Where, err)
	}
	name := UnitFileName(mnt.Where)
	if _, err := m.Runner.Run(ctx, "systemctl", "start", name); err != nil {
		return fmt.Errorf("pool: starting mount unit for %s: %w", mnt.Where, err)
	}
	return nil
}

// Unmount stops mnt's .mount unit. Busy failures retry within
// unmountRetryWindow (issue #332); a where that is already not a mount
// point is success.
func (m SystemdMounter) Unmount(ctx context.Context, where string) error {
	now := m.Now
	if now == nil {
		now = time.Now
	}
	sleep := m.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	isMountpoint := m.asDirect().isMountpoint()
	name := UnitFileName(where)

	deadline := now().Add(unmountRetryWindow)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("pool: unmounting %s: %w", where, err)
		}
		_, err := m.Runner.Run(ctx, "systemctl", "stop", name)
		if err == nil {
			return nil
		}
		if systemdStopShouldRetry(err) && now().Before(deadline) {
			sleep(unmountRetryDelay)
			continue
		}
		mounted, checkErr := isMountpoint(where)
		if checkErr == nil && !mounted {
			return nil
		}
		if checkErr != nil && errors.Is(checkErr, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("pool: unmounting %s: %w", where, err)
	}
}

// Remount stops the live unit then starts mnt (doc 02 §4 "Adding a
// disk" step 6). On a mount failure it rolls previous back up.
func (m SystemdMounter) Remount(ctx context.Context, previous, mnt Mount) error {
	if err := m.Unmount(ctx, mnt.Where); err != nil {
		return err
	}
	if err := m.Mount(ctx, mnt); err != nil {
		if rbErr := m.Mount(ctx, previous); rbErr != nil {
			return fmt.Errorf("%w (rollback to the previous mount also failed: %v)", err, rbErr)
		}
		return err
	}
	return nil
}
