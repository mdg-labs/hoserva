package config

import (
	"context"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// WriteDiskMounts writes each physical-disk systemd .mount unit
// (disk.MountUnit.Render, doc 01 §6, Q21) through Write. The body is
// exactly Render's output — this does not recompute What=/dev/disk/by-uuid
// or Options=. command names the `hoserva <command>` a user runs instead
// of hand-editing the unit files, per the doc 01 §2 header.
func (g *Generator) WriteDiskMounts(ctx context.Context, units []disk.MountUnit, command string, revision int, now time.Time) error {
	for _, u := range units {
		file := File{
			Path:    poolMountUnitDir + disk.UnitFileName(u.Where),
			Command: command,
			Body:    []byte(u.Render()),
		}
		if err := g.Write(ctx, file, revision, now); err != nil {
			return err
		}
	}
	return nil
}
