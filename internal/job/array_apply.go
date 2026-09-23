package job

import (
	"context"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// applyArrayFromStore generates disk mount units, mergerfs pool units and
// snapraid.conf from SQLite topology (D4, D1) and mounts each physical
// disk by the filesystem UUID stored there (Q21). Rewriting units for a
// disk already mounted is success, not a second format — and, since #288
// (disk_add/disk_replace call this a second time against disks
// create-array's own call already mounted), not a second, stacked kernel
// mount either: a mountpoint this call finds already mounted is left
// alone rather than mounted again on top of itself. It never reads
// job-params JSON and never formats.
func applyArrayFromStore(ctx context.Context, st *store.ArrayStore, g *config.Generator, mounter disk.UnitMounter, now time.Time) error {
	settings, disks, err := st.GetArray(ctx)
	if err != nil {
		return err
	}

	units, err := mountUnitsFromStore(disks)
	if err != nil {
		return err
	}
	if err := g.WriteDiskMounts(ctx, units, arrayCreateCommand, 1, now); err != nil {
		return err
	}

	poolState := poolStateFromStore(settings, disks)
	if err := g.WritePoolMounts(ctx, poolState, arrayCreateCommand, 1, now); err != nil {
		return err
	}

	body, err := layoutFromStore(disks).Render()
	if err != nil {
		return err
	}
	if err := g.Write(ctx, config.File{
		Path:    "snapraid.conf",
		Command: arrayCreateCommand,
		Body:    []byte(body),
	}, 1, now); err != nil {
		return err
	}

	for _, u := range units {
		if alreadyMounted(u.Where) {
			continue
		}
		if err := mounter.Mount(ctx, u); err != nil {
			return err
		}
	}
	return nil
}

// alreadyMounted reports whether where is already a real mountpoint.
// Any error (most commonly the mountpoint directory not existing yet, the
// ordinary case for a disk's first-ever mount) is treated as "not
// mounted" rather than propagated — this check only ever skips a Mount
// call it is positively certain is already satisfied; anything less
// certain still goes through Mount exactly as before.
func alreadyMounted(where string) bool {
	mounted, err := disk.IsMountpoint(where)
	return err == nil && mounted
}

func mountUnitsFromStore(disks []store.ArrayDisk) ([]disk.MountUnit, error) {
	units := make([]disk.MountUnit, 0, len(disks))
	for _, d := range disks {
		if d.FSUUID == "" {
			return nil, fmt.Errorf("job: array disk %s has no filesystem UUID", d.Device)
		}
		if d.Mountpoint == "" {
			return nil, fmt.Errorf("job: array disk %s has no mountpoint", d.Device)
		}
		units = append(units, disk.MountUnit{
			Where:       d.Mountpoint,
			UUID:        d.FSUUID,
			Filesystem:  disk.FilesystemType(d.Filesystem),
			Description: diskMountDescription(d),
		})
	}
	return units, nil
}

func diskMountDescription(d store.ArrayDisk) string {
	switch d.Role {
	case store.ArrayRoleData:
		return fmt.Sprintf("Hoserva data disk %d", d.RoleIndex)
	case store.ArrayRoleParity:
		return fmt.Sprintf("Hoserva parity disk %d", d.RoleIndex)
	case store.ArrayRoleCache:
		return "Hoserva cache disk"
	default:
		return fmt.Sprintf("Hoserva %s disk", d.Role)
	}
}

func poolStateFromStore(settings store.ArraySettings, disks []store.ArrayDisk) config.PoolState {
	var dataDisks []string
	var cachePath string
	for _, d := range disks {
		switch d.Role {
		case store.ArrayRoleData:
			dataDisks = append(dataDisks, d.Mountpoint)
		case store.ArrayRoleCache:
			cachePath = d.Mountpoint
		}
	}
	return config.PoolState{
		DataDisks:    dataDisks,
		CachePath:    cachePath,
		CreatePolicy: pool.CreatePolicy(settings.CreatePolicy),
		Options:      pool.Options{MinFreeSpace: settings.MinFreeSpace},
	}
}

func layoutFromStore(disks []store.ArrayDisk) parity.Layout {
	var l parity.Layout
	for _, d := range disks {
		switch d.Role {
		case store.ArrayRoleParity:
			l.ParityMounts = append(l.ParityMounts, d.Mountpoint)
		case store.ArrayRoleData:
			l.DataMounts = append(l.DataMounts, d.Mountpoint)
		case store.ArrayRoleCache:
			l.CacheMount = d.Mountpoint
		}
	}
	return l
}
