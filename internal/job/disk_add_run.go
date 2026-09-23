package job

import (
	"context"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

// DiskAddDeps is everything RunDiskAdd needs to add a data disk to an
// already-existing array (doc 02 §4 "Adding a disk"): the same
// dependencies DiskFormatDeps uses for create-array, since adding a disk
// to a live array regenerates the exact same generated files from the
// exact same SQLite state (D4).
type DiskAddDeps struct {
	Provider  disk.Provider
	Runner    disk.Runner
	Store     *store.ArrayStore
	Generator *config.Generator
	Mounter   disk.UnitMounter
	// ArrayReady, when set, runs once applyArrayFromStore has persisted
	// the new disk and mounted it — the same #262/#263 rebuild
	// DiskFormatDeps.ArrayReady performs for create-array, so a live
	// disk_add leaves job.ArraySequence (and so `array start`,
	// `GET /pool`) reflecting the new disk without a daemon restart.
	ArrayReady func(ctx context.Context) error
	// Now, when set, stamps generated-file headers; nil uses time.Now.
	Now func() time.Time
}

// RunDiskAdd is the RunFunc hoservad registers for TypeDiskAdd. It
// re-validates the queued plan's confirmation and Q19/Q20/Q23 checks
// against the array's current topology — a queued job cannot skip the
// guard the addDisk handler already enforced (doc 03 §3.1 step 6, extended
// to this job type) — formats or adopts the disk through
// disk.FormatForAddition (#32: re-checks identity and refuses the boot
// disk at format time, not just at plan time), inserts one array_disks
// row at the next free /mnt/diskN, and regenerates mount units, the pool
// and snapraid.conf from SQLite exactly as applyArrayFromStore already
// does for create-array. A failure before AddDataDisk leaves no new row
// and no regenerated files, aside from the disk itself already having been
// formatted or adopted. A failure after AddDataDisk succeeds cannot be
// retried by resubmitting disk_add for the same device: the row already
// exists, so ValidateDiskAddition refuses it again (as a duplicate device
// assignment, or — once the disk carries a WWN or serial — as an existing
// array member, ErrDiskAlreadyMember), and it no longer shows as
// unassigned on the pool page either. array_disks is the source of truth
// (D4), so recovery from here never edits that row: a failed
// generated-file write leaves the new disk's own mount unit missing,
// fixed by the next applyArrayFromStore (create-array, or another
// disk_add); a Mount failure already leaves the units and snapraid.conf
// regenerated, so restarting hoservad rebuilds job.ArraySequence from the
// store and `array start` then mounts the disk; an ArrayReady failure is
// likewise fixed by a daemon restart, since AddDataDisk and
// applyArrayFromStore have both already succeeded by the time it runs.
func RunDiskAdd(d DiskAddDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		params, err := decodeDiskAddParams(rc.Params())
		if err != nil {
			return err
		}
		if d.Store == nil || d.Generator == nil || d.Mounter == nil || d.Provider == nil || d.Runner == nil {
			return fmt.Errorf("job: disk_add is missing dependencies")
		}

		_, disks, err := d.Store.GetArray(ctx)
		if err != nil {
			return err
		}

		if SingleDiskConfirmation(params.Disk) != params.Confirmation {
			return disk.ErrConfirmationMismatch
		}

		// A fresh inventory read, not whatever params.Disk carried at
		// submit time: this job can have sat queued behind a running
		// storage-class job for as long as that job ran, and the physical
		// disk at params.Disk.Device can have changed in that window —
		// including another array member renumbered onto this exact path.
		// confirmTargetIdentityUnchanged refuses the job outright on any
		// difference, so the target actually formatted below is always the
		// one this fresh listing just confirmed, never the submit-time one
		// alone.
		listed, err := d.Provider.List(ctx)
		if err != nil {
			return fmt.Errorf("job: listing disks to confirm %s's current identity: %w", params.Disk.Device, err)
		}
		target, err := confirmTargetIdentityUnchanged(listed, params.Disk)
		if err != nil {
			return err
		}
		if err := ValidateDiskAddition(disks, target, params.Sizes); err != nil {
			return err
		}

		if err := disk.FormatForAddition(ctx, d.Provider, d.Runner, disk.DiskAddition{
			Device:       target.Device,
			Filesystem:   target.Filesystem,
			Adopt:        target.Adopt,
			WWN:          target.WWN,
			Serial:       target.Serial,
			WeakIdentity: target.WeakIdentity,
			ByIDName:     target.ByIDName,
		}); err != nil {
			return err
		}

		uuid, err := disk.FilesystemUUID(ctx, d.Runner, formatTargetOf(params.Disk))
		if err != nil {
			return err
		}

		mountpoint := disk.NextDataMountpoint(mountpointsOf(disks))
		roleIndex, err := dataMountRoleIndex(mountpoint)
		if err != nil {
			return err
		}

		if err := d.Store.AddDataDisk(ctx, store.ArrayDisk{
			Role:         store.ArrayRoleData,
			RoleIndex:    roleIndex,
			Device:       params.Disk.Device,
			Filesystem:   string(params.Disk.Filesystem),
			FSUUID:       uuid,
			WWN:          params.Disk.WWN,
			Serial:       params.Disk.Serial,
			ByIDName:     params.Disk.ByIDName,
			WeakIdentity: params.Disk.WeakIdentity,
			Mountpoint:   mountpoint,
		}); err != nil {
			return err
		}

		now := time.Now
		if d.Now != nil {
			now = d.Now
		}
		if err := applyArrayFromStore(ctx, d.Store, d.Generator, d.Mounter, now()); err != nil {
			return err
		}
		if d.ArrayReady == nil {
			return nil
		}
		return d.ArrayReady(ctx)
	}
}
