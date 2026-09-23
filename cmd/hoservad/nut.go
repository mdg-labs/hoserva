package main

import (
	"context"
	"fmt"
	"strings"

	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
)

// nutReloader restarts the NUT systemd units that match a UPS connection
// mode after WriteUPS lands new files (#249, Q77). USB (standalone)
// restarts the local driver, upsd and upsmon; network (netclient) only
// restarts upsmon. Units that are not installed are skipped the same way
// disk.ServiceUnitController skips Samba/NFS when their packages are
// absent — NUT is a Recommends, not a hard Depends.
type nutReloader struct {
	Runner disk.Runner
}

func newNUTReloader(runner disk.Runner) *nutReloader {
	return &nutReloader{Runner: runner}
}

func (r *nutReloader) Reload(ctx context.Context, connection cfggen.UPSConnection) error {
	units := []string{"nut-monitor.service"}
	if connection == cfggen.UPSConnectionUSB {
		units = []string{
			"nut-driver-enumerator.service",
			"nut-driver@" + cfggen.UPSLocalName + ".service",
			"nut-server.service",
			"nut-monitor.service",
		}
	}
	for _, unit := range units {
		if err := r.restartIfInstalled(ctx, unit); err != nil {
			return err
		}
	}
	return nil
}

func (r *nutReloader) restartIfInstalled(ctx context.Context, unit string) error {
	out, err := r.Runner.Run(ctx, "systemctl", "show", "--property=LoadState", "--value", unit)
	if err != nil {
		return fmt.Errorf("checking %s: %w", unit, err)
	}
	if strings.TrimSpace(string(out)) == "not-found" {
		return nil
	}
	if _, err := r.Runner.Run(ctx, "systemctl", "restart", unit); err != nil {
		return fmt.Errorf("restarting %s: %w", unit, err)
	}
	return nil
}
