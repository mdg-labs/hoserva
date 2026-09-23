package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// newArraySequence constructs the daemon's single job.ArraySequence from
// persisted topology (doc 02 §4, Q70, Q69). No array yet returns (nil,
// nil) so stop/start stay 501 rather than unmounting an empty path.
// Forgetting Evaluate here would leave StorageGate unready and refuse
// every Start even with every disk present; skipping the gate would
// mount a degraded array.
//
// shares populates ShareMounts with every persisted share's own mount
// and, for every non-cache-only share, its mover write target (#268):
// without this, Stop never unmounts a share's own mergerfs mount before
// the catch-all, so the catch-all unmount fails EBUSY the moment any
// share exists, and Start never remounts a share at all — a share that
// survives to a reboot loses its own mount and its mover write target
// until something other than array start remounts it by hand.
//
// Services always carries Samba and NFS (doc 02 §4's "Samba and NFS
// stop"/"start" step, #309) — even when there are no data mounts yet —
// so ArraySequence.Stop always stops them before any unmount can run,
// and every caller (array stop, the UPS low-battery shutdown, the
// update reboot) gets the same ordering because they all run this one
// sequence.
//
// The readiness gate also reports not ready while a data-disk upgrade is
// pending (doc 02 §4 UR2), and Start confirms every mounted array disk
// against the filesystem UUID SQLite names before anything above the
// disks starts (UR9).
func newArraySequence(ctx context.Context, scheduler *job.Scheduler, arrays *store.ArrayStore, shares *store.ShareStore, disks disk.Provider, runner disk.Runner) (*job.ArraySequence, error) {
	settings, assigned, err := arrays.GetArray(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNoArray) {
			return nil, nil
		}
		return nil, fmt.Errorf("loading array topology: %w", err)
	}

	expected := make([]disk.ExpectedDisk, 0, len(assigned))
	diskMounts := make([]job.ArrayMount, 0, len(assigned))
	var dataMounts []string
	var cachePath string
	for _, d := range assigned {
		if d.Mountpoint == "" {
			return nil, fmt.Errorf("array disk %s has no mountpoint", d.Device)
		}
		expected = append(expected, disk.ExpectedDisk{
			Identity: disk.Identity{
				WWN:          d.WWN,
				Serial:       d.Serial,
				WeakIdentity: d.WeakIdentity,
				ByIDName:     d.ByIDName,
			},
			Role:    d.Role,
			MountAt: d.Mountpoint,
		})
		diskMounts = append(diskMounts, disk.MountUnitController{
			Unit: disk.MountUnit{
				Where:      d.Mountpoint,
				UUID:       d.FSUUID,
				Filesystem: disk.FilesystemType(d.Filesystem),
			},
			Runner: runner,
		})
		switch d.Role {
		case store.ArrayRoleData:
			dataMounts = append(dataMounts, d.Mountpoint)
		case store.ArrayRoleCache:
			cachePath = d.Mountpoint
		}
	}

	gate := disk.NewStorageGate(expected)
	listed, err := disks.List(ctx)
	if err != nil {
		// Leave the gate unevaluated (Ready is false until Evaluate).
		// Returning the error would abort daemon startup and take the API,
		// diagnostics, and Stop with it; Start already refuses with
		// storage_not_ready while the gate is unready.
		log.Printf("hoservad: listing disks for storage gate: %v — Start will refuse until inventory can be evaluated", err)
	} else {
		present := make([]disk.Identity, 0, len(listed))
		for _, d := range listed {
			present = append(present, disk.Identity{
				WWN:          d.WWN,
				Serial:       d.Serial,
				WeakIdentity: d.WeakIdentity,
				ByIDName:     d.ByIDName,
			})
		}
		gate.Evaluate(present)
	}

	var checked []disk.MountUnit
	for _, d := range assigned {
		checked = append(checked, disk.MountUnit{Where: d.Mountpoint, UUID: d.FSUUID})
	}
	seq := &job.ArraySequence{
		Scheduler: scheduler,
		Gate:      job.PendingUpgradeGate{Gate: gate, Scheduler: scheduler},
		DiskCheck: job.ArrayDiskUUIDCheck{Mounts: disk.KernelMounts{Runner: runner}, Disks: checked},
		Services: []job.ArrayService{
			disk.ServiceUnitController{ServiceName: "Samba", Unit: cfggen.SambaServiceUnit, Runner: runner},
			disk.ServiceUnitController{ServiceName: "NFS", Unit: cfggen.NFSServiceUnit, Runner: runner},
		},
		Disks: diskMounts,
	}
	if len(dataMounts) == 0 {
		return seq, nil
	}

	catchAll, err := pool.CatchAllMount(dataMounts, pool.Options{MinFreeSpace: settings.MinFreeSpace})
	if err != nil {
		return nil, fmt.Errorf("building catch-all pool mount: %w", err)
	}
	if settings.CreatePolicy != "" {
		catchAll.CreatePolicy = pool.CreatePolicy(settings.CreatePolicy)
	}
	seq.CatchAll = pool.MountController{
		Mnt:     catchAll,
		Mounter: pool.Mounter{Runner: runner},
	}

	rows, err := shares.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading shares: %w", err)
	}
	opts := pool.Options{MinFreeSpace: settings.MinFreeSpace}
	for _, row := range rows {
		sh := pool.Share{
			Name:         row.Name,
			CacheMode:    pool.CacheMode(row.CacheMode),
			CreatePolicy: pool.CreatePolicy(row.CreatePolicy),
		}
		shareMount, err := pool.ShareMount(sh, dataMounts, cachePath, opts)
		if err != nil {
			return nil, fmt.Errorf("building share mount for %q: %w", sh.Name, err)
		}
		seq.ShareMounts = append(seq.ShareMounts, pool.MountController{
			Mnt:     shareMount,
			Mounter: pool.Mounter{Runner: runner},
		})
		if sh.CacheMode == pool.CacheOnly {
			continue
		}
		moverMount, err := pool.MoverTargetMount(sh, dataMounts, opts)
		if err != nil {
			return nil, fmt.Errorf("building mover target mount for %q: %w", sh.Name, err)
		}
		seq.ShareMounts = append(seq.ShareMounts, pool.MountController{
			Mnt:     moverMount,
			Mounter: pool.Mounter{Runner: runner},
		})
	}
	return seq, nil
}
