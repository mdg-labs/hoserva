package main

import (
	"context"
	"errors"
	"fmt"

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
func newArraySequence(ctx context.Context, scheduler *job.Scheduler, arrays *store.ArrayStore, disks disk.Provider, runner disk.Runner) (*job.ArraySequence, error) {
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
		if d.Role == store.ArrayRoleData {
			dataMounts = append(dataMounts, d.Mountpoint)
		}
	}

	listed, err := disks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing disks for storage gate: %w", err)
	}
	present := make([]disk.Identity, 0, len(listed))
	for _, d := range listed {
		present = append(present, disk.Identity{
			WWN:          d.WWN,
			Serial:       d.Serial,
			WeakIdentity: d.WeakIdentity,
			ByIDName:     d.ByIDName,
		})
	}
	gate := disk.NewStorageGate(expected)
	gate.Evaluate(present)

	seq := &job.ArraySequence{
		Scheduler: scheduler,
		Gate:      gate,
		Disks:     diskMounts,
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
	return seq, nil
}
