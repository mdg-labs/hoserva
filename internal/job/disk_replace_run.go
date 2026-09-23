package job

import (
	"context"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
)

// DiskReplaceDeps is everything RunDiskReplace needs to replace a failed
// data disk (doc 02 §4 "Replacing a failed disk"): the same topology
// dependencies DiskAddDeps uses, plus the SnapRAID engine that reconstructs
// the replacement's contents from parity once it is formatted and
// mounted back at the failed disk's own slot.
type DiskReplaceDeps struct {
	Provider  disk.Provider
	Runner    disk.Runner
	Store     *store.ArrayStore
	Generator *config.Generator
	Mounter   disk.UnitMounter
	Parity    parity.Engine
	// ArrayReady, when set, runs once applyArrayFromStore has switched the
	// slot over to the replacement and mounted it, before the fix step —
	// the same #262/#263 rebuild DiskFormatDeps.ArrayReady performs for
	// create-array, so a live disk_replace leaves job.ArraySequence (and
	// so `array start`, `GET /pool`) reflecting the replacement without a
	// daemon restart, even if the fix step itself then fails or is
	// cancelled.
	ArrayReady func(ctx context.Context) error
	// Now, when set, stamps generated-file headers; nil uses time.Now.
	Now func() time.Time
}

// RunDiskReplace is the RunFunc hoservad registers for TypeDiskReplace. It
// re-validates the queued plan's confirmation and Q19/Q20/Q23 checks
// against the array's current topology (the same defense-in-depth
// RunDiskAdd applies), refuses when the slot's own recorded disk is still
// mounted or still present in inventory by identity
// (ConfirmReplacementTargetAbsent, doc 02 §4 steps 1-2 — a disk that has
// not actually failed or been removed is not this job's job, #289), formats
// or adopts the replacement through disk.FormatForAddition at the failed
// disk's own mountpoint, re-points that slot's array_disks row at the
// replacement's identity (store.ReplaceDataDisk), regenerates mount units,
// the pool and snapraid.conf from SQLite, confirms the mountpoint is
// genuinely backed by the replacement's own filesystem before touching
// parity, and only then resolves the slot's own SnapRAID disk label from
// the regenerated config (parity.ParityStatus.DataDiskLabel) and runs
// `snapraid fix` to reconstruct its contents from parity and the
// remaining disks.
//
// A failure before ReplaceDataDisk leaves the array's topology unchanged,
// aside from the replacement disk itself already having been formatted or
// adopted. A failure between ReplaceDataDisk and a successful Fix — the
// fix step itself failing, or the job being interrupted mid-fix — leaves
// the array's topology, mounts and snapraid.conf already switched over to
// the replacement, recoverable by an ordinary `hoserva fix` against the
// same disk: neither ReplaceDataDisk nor applyArrayFromStore ever touch
// parity or another disk's own data, and SnapRAID's own fix only ever
// reconstructs files it has not already restored, so a second fix against
// the same disk picks up exactly where an interrupted one left off.
func RunDiskReplace(d DiskReplaceDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		params, err := decodeDiskReplaceParams(rc.Params())
		if err != nil {
			return err
		}
		if d.Store == nil || d.Generator == nil || d.Mounter == nil || d.Provider == nil || d.Runner == nil || d.Parity == nil {
			return fmt.Errorf("job: disk_replace is missing dependencies")
		}

		_, disks, err := d.Store.GetArray(ctx)
		if err != nil {
			return err
		}
		oldDisk, err := d.Store.GetDataDiskByMountpoint(ctx, params.Mountpoint)
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
		// including another array member renumbered onto this exact path —
		// the same reason ConfirmReplacementTargetAbsent below needs a
		// fresh listing of its own, reused here rather than listed twice.
		// confirmTargetIdentityUnchanged refuses the job outright on any
		// difference, so the replacement actually formatted below is
		// always the one this fresh listing just confirmed.
		listed, err := d.Provider.List(ctx)
		if err != nil {
			return fmt.Errorf("job: listing disks to confirm %s's own disk is gone: %w", params.Mountpoint, err)
		}
		target, err := confirmTargetIdentityUnchanged(listed, params.Disk)
		if err != nil {
			return err
		}
		if err := ValidateDiskReplacement(disks, params.Mountpoint, target, params.Sizes); err != nil {
			return err
		}
		if err := ConfirmReplacementTargetAbsent(params.Mountpoint, oldDisk, listed); err != nil {
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

		row := store.ArrayDisk{
			Device:       params.Disk.Device,
			Filesystem:   string(params.Disk.Filesystem),
			FSUUID:       uuid,
			WWN:          params.Disk.WWN,
			Serial:       params.Disk.Serial,
			ByIDName:     params.Disk.ByIDName,
			WeakIdentity: params.Disk.WeakIdentity,
		}
		if size, ok := params.Sizes[params.Disk.Device]; ok {
			row.Size = size
			row.SizeSet = true
		}
		if err := d.Store.ReplaceDataDisk(ctx, params.Mountpoint, row); err != nil {
			return err
		}

		now := time.Now
		if d.Now != nil {
			now = d.Now
		}
		if err := applyArrayFromStore(ctx, d.Store, d.Generator, d.Mounter, now()); err != nil {
			return err
		}
		if d.ArrayReady != nil {
			if err := d.ArrayReady(ctx); err != nil {
				return err
			}
		}

		// The mountpoint must genuinely be backed by the replacement's own
		// filesystem before SnapRAID's fix ever runs against it — the
		// safety check this job's own regression test (finding 1) proves:
		// without it, a slot whose real disk somehow never actually
		// switched over would run fix against the wrong filesystem and
		// report success.
		mountedUUID, err := disk.MountedUUID(ctx, d.Runner, params.Mountpoint)
		if err != nil {
			return fmt.Errorf("job: confirming %s is mounted on the replacement before running fix: %w", params.Mountpoint, err)
		}
		if mountedUUID != uuid {
			return fmt.Errorf("job: %s is mounted on filesystem %s, not the replacement's %s — refusing to run snapraid fix", params.Mountpoint, mountedUUID, uuid)
		}

		status, err := d.Parity.Status(ctx)
		if err != nil {
			return fmt.Errorf("job: reading parity status to resolve %s's SnapRAID label: %w", params.Mountpoint, err)
		}
		label, ok := status.DataDiskLabel(params.Mountpoint)
		if !ok {
			return fmt.Errorf("job: no SnapRAID data disk label for %s", params.Mountpoint)
		}

		ch, err := d.Parity.Fix(ctx, parity.FixOpts{Disk: label})
		return drainProgress(ch, err)
	}
}
