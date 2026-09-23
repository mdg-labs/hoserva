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

// DiskUpgradeParityDeps is everything RunDiskUpgradeParity needs to
// upgrade a parity disk to a larger one (doc 02 §4 "Larger parity disk",
// #116, #289).
type DiskUpgradeParityDeps struct {
	Provider  disk.Provider
	Runner    disk.Runner
	Store     *store.ArrayStore
	Generator *config.Generator
	// Mounter regenerates and, if not already mounted, mounts the new
	// parity disk's own persistent unit once the configuration switches to
	// it — SystemdMounter in production.
	Mounter disk.UnitMounter
	// UpgradeMounter is the new parity disk's own initial mount, before the
	// switch — disk.DirectMounter in production, a scriptable
	// disk.FakeMounter in tests, DiskUpgradeDataDeps.UpgradeMounter's own
	// reasoning.
	UpgradeMounter disk.UnitMounter
	Parity         parity.Engine
	// ArrayReady, when set, runs once the switch has been regenerated and
	// mounted — DiskUpgradeDataDeps.ArrayReady's own convention.
	ArrayReady func(ctx context.Context) error
	// Now, when set, stamps generated-file headers; nil uses time.Now.
	Now func() time.Time
}

// RunDiskUpgradeParity is the RunFunc hoservad registers for
// TypeDiskUpgradeParity. Like RunDiskUpgradeData, it only re-validates the
// queued plan's confirmation, identity and Q19/Q20/Q23 checks on this
// job's first invocation (rc.InitialCheckpoint() empty): formatting the
// new parity disk — gated to that same first invocation, since a resumed
// run must never reformat a disk that may already hold a partial parity-
// file copy — gives it a real filesystem UUID it did not have at submit
// time. Every invocation (fresh or resumed) re-reads that UUID and mounts
// params.NewMountpoint if it is not already mounted — a raw kernel mount
// a prior invocation made survives a daemon restart on its own (only a
// reboot drops it), so this only ever repairs the reboot case, the same
// property disk.RunDataDiskUpgrade's own ensureStagingMounted protects for
// its Staging path.
//
// parity.RunParityUpgrade itself drives copying and byte-for-byte
// verification (#116); this wrapper supplies its three hooks. ApplyLayout
// — called once, after Verifying passes and before `snapraid check` ever
// runs against the new disk — is where the array's own topology commits
// to it: store.ArrayStore's own parity-slot update re-points the slot's
// role_index at the new disk's identity and mountpoint, and
// applyArrayFromStore regenerates mount units, the pool and
// snapraid.conf from SQLite (D4). This has to happen before Checking, not
// after: `snapraid check` only ever verifies whatever configuration is
// currently on disk, and disk.RunParityUpgrade's own phase order runs
// Checking immediately after ApplyLayout, never before. A Check that then
// fails leaves the switch already committed but the old parity file
// completely untouched and still mountable — recoverable the ordinary way
// (doc 02 §4 "Replacing a failed disk"), the same residual window this
// package's own doc comment on ParityUpgradePhase already documents and
// accepts. Release, reached only once Check passes, does no further store
// write of its own — the switch already happened — and only logs that the
// old parity disk is now free.
func RunDiskUpgradeParity(d DiskUpgradeParityDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		params, err := decodeDiskUpgradeParityParams(rc.Params())
		if err != nil {
			return err
		}
		if d.Store == nil || d.Generator == nil || d.Mounter == nil || d.UpgradeMounter == nil || d.Provider == nil || d.Runner == nil || d.Parity == nil {
			return fmt.Errorf("job: disk_upgrade_parity is missing dependencies")
		}

		if SingleDiskConfirmation(params.Disk) != params.Confirmation {
			return disk.ErrConfirmationMismatch
		}

		_, disks, err := d.Store.GetArray(ctx)
		if err != nil {
			return err
		}
		fresh := len(rc.InitialCheckpoint()) == 0
		oldDisk, switched, err := parityUpgradeSlot(disks, params, fresh)
		if err != nil {
			return err
		}

		if fresh {
			listed, err := d.Provider.List(ctx)
			if err != nil {
				return fmt.Errorf("job: listing disks to confirm %s's current identity: %w", params.Disk.Device, err)
			}
			// By-id identity only, never the filesystem UUID: the first
			// checkpoint is saved only once the copy completes, so a run
			// interrupted anywhere between the format and that point
			// resumes here with the disk already formatted. Reformatting
			// it is safe — it holds nothing but a partial parity copy —
			// and Q21 refuses a weak-identity parity disk, so WWN and
			// serial always identify it.
			if err := confirmByIDIdentity(listed, params.Disk); err != nil {
				return err
			}
			target := params.Disk
			// See RunDiskUpgradeData's own identical check: ValidateParityDiskUpgrade's
			// "remaining" set excludes oldDisk's own row, so it alone
			// cannot catch a target that already is oldDisk's own current
			// device — defense in depth against reformatting a disk that
			// is already this slot's live occupant.
			if err := refuseKnownIdentity([]store.ArrayDisk{oldDisk}, target); err != nil {
				return fmt.Errorf("job: %s already occupies %s — nothing to upgrade: %w", target.Device, params.Mountpoint, err)
			}
			if err := ValidateParityDiskUpgrade(disks, params.Mountpoint, target, params.Sizes); err != nil {
				return err
			}
			if err := disk.FormatForAddition(ctx, d.Provider, d.Runner, disk.DiskAddition{
				Device:       target.Device,
				Filesystem:   target.Filesystem,
				WWN:          target.WWN,
				Serial:       target.Serial,
				WeakIdentity: target.WeakIdentity,
				ByIDName:     target.ByIDName,
			}); err != nil {
				return err
			}
		}

		uuid, err := disk.FilesystemUUID(ctx, d.Runner, formatTargetOf(params.Disk))
		if err != nil {
			return fmt.Errorf("job: reading the new parity disk's filesystem UUID: %w", err)
		}
		newUnit := disk.MountUnit{
			Where:       params.NewMountpoint,
			UUID:        uuid,
			Filesystem:  params.Disk.Filesystem,
			Description: fmt.Sprintf("Hoserva parity disk %d", oldDisk.RoleIndex),
		}
		if !alreadyMounted(params.NewMountpoint) {
			if err := d.UpgradeMounter.Mount(ctx, newUnit); err != nil {
				return fmt.Errorf("job: mounting the new parity disk at %s: %w", params.NewMountpoint, err)
			}
		}
		// Before ever copying the parity file, confirm NewMountpoint is
		// genuinely backed by the new disk, not a stale, still-mounted
		// disk left over at that same path: a slot an
		// earlier upgrade never unmounted, or a plan computed against a
		// snapshot that no longer matches what's physically there, would
		// otherwise silently read, overwrite and "release" the wrong
		// physical disk. No remount is attempted here — a mismatch this
		// job did not itself just create is refused outright, never
		// worked around.
		if err := confirmMountedUUID(ctx, d.Runner, newUnit); err != nil {
			return fmt.Errorf("job: confirming the new parity disk is mounted at %s: %w", params.NewMountpoint, err)
		}

		spec := parity.ParityUpgradeSpec{
			OldParityPath: parity.ParityFilePath(oldDisk.Mountpoint),
			NewParityPath: parity.ParityFilePath(params.NewMountpoint),
			NewLayout:     layoutFromStore(disksWithParitySwapped(disks, params.Mountpoint, params.NewMountpoint)),
		}

		deps := parity.ParityUpgradeDeps{
			ApplyLayout: func(ctx context.Context, _ parity.Layout) error {
				if switched {
					// An earlier invocation committed the switch and was
					// interrupted before checkpointing past it; only the
					// regeneration below still needs to run.
					return d.regenerateAfterSwitch(ctx)
				}
				if err := d.Store.UpgradeParityDisk(ctx, params.Mountpoint, store.ArrayDisk{
					Role:         store.ArrayRoleParity,
					RoleIndex:    oldDisk.RoleIndex,
					Device:       params.Disk.Device,
					Filesystem:   string(params.Disk.Filesystem),
					FSUUID:       uuid,
					WWN:          params.Disk.WWN,
					Serial:       params.Disk.Serial,
					ByIDName:     params.Disk.ByIDName,
					WeakIdentity: params.Disk.WeakIdentity,
					Mountpoint:   params.NewMountpoint,
				}); err != nil {
					return err
				}
				return d.regenerateAfterSwitch(ctx)
			},
			Check: func(ctx context.Context, opts parity.CheckOpts) (<-chan parity.Progress, error) {
				return d.Parity.Check(ctx, opts)
			},
			Release: func(ctx context.Context) error {
				// Actually unmount the old parity disk rather than only
				// logging that it's "released": left
				// mounted, its own mountpoint stays occupied forever, so
				// the next parity upgrade's own NextParityMountpoint (a
				// dual-parity array's normal next step, Q20) picks it
				// again, finds it already mounted and skips remounting
				// it — then copies, verifies and switches the
				// configuration onto this now-retired disk instead of
				// the disk it actually formatted.
				if err := d.UpgradeMounter.Unmount(ctx, disk.MountUnit{Where: params.Mountpoint}); err != nil {
					return fmt.Errorf("job: unmounting the old parity disk at %s: %w", params.Mountpoint, err)
				}
				_, _ = fmt.Fprintf(rc.Output(), "parity disk upgrade: %s released; its own parity file is untouched\n", params.Mountpoint)
				return nil
			},
		}

		hooks := parity.ParityUpgradeHooks{
			StopRequested:  rc.StopRequested(),
			SaveCheckpoint: rc.SaveCheckpoint,
			SetProgress:    rc.SetProgress,
			Log: func(format string, args ...any) {
				_, _ = fmt.Fprintf(rc.Output(), format+"\n", args...)
			},
		}

		_, err = parity.RunParityUpgrade(ctx, spec, deps, hooks, rc.InitialCheckpoint())
		return err
	}
}

// regenerateAfterSwitch rewrites every generated file from SQLite once the
// parity slot names the new disk, mounts its unit and rebuilds the array
// sequence. It is idempotent, so a resume may run it again.
func (d DiskUpgradeParityDeps) regenerateAfterSwitch(ctx context.Context) error {
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

// parityUpgradeSlot finds the parity slot this upgrade works on. Before
// ApplyLayout it is the row at params.Mountpoint. After it — only ever on
// a resumed invocation — the row has moved to params.NewMountpoint and
// names params.Disk; it is returned with its old mountpoint restored, so
// the old parity file path and Release still address the old disk, and
// switched reports that the store write must not run again.
func parityUpgradeSlot(disks []store.ArrayDisk, params DiskUpgradeParityParams, fresh bool) (store.ArrayDisk, bool, error) {
	if row, ok := arrayDiskAtMountpoint(disks, params.Mountpoint); ok && row.Role == store.ArrayRoleParity {
		return row, false, nil
	}
	if !fresh {
		row, ok := arrayDiskAtMountpoint(disks, params.NewMountpoint)
		want := params.Disk
		if ok && row.Role == store.ArrayRoleParity && row.Device == want.Device &&
			row.WWN == want.WWN && row.Serial == want.Serial && row.ByIDName == want.ByIDName {
			row.Mountpoint = params.Mountpoint
			return row, true, nil
		}
	}
	return store.ArrayDisk{}, false, fmt.Errorf("job: no parity disk at %s", params.Mountpoint)
}

// disksWithParitySwapped returns a copy of disks with the parity row at
// oldMountpoint moved to newMountpoint — the layout RunDiskUpgradeParity's
// own ParityUpgradeSpec.NewLayout previews before the store itself has
// switched over; ApplyLayout re-derives the actual configuration from
// SQLite once it runs (D4), so this only ever needs to be an accurate
// preview, not a second source of truth.
func disksWithParitySwapped(disks []store.ArrayDisk, oldMountpoint, newMountpoint string) []store.ArrayDisk {
	out := make([]store.ArrayDisk, len(disks))
	copy(out, disks)
	for i, d := range out {
		if d.Role == store.ArrayRoleParity && d.Mountpoint == oldMountpoint {
			out[i].Mountpoint = newMountpoint
		}
	}
	return out
}
