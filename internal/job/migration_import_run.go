package job

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// migrationImportUndoTimeout bounds the undo of a failed adoption, which runs
// without the request's or the job's own cancellation.
const migrationImportUndoTimeout = 2 * time.Minute

// MigrationImportDeps is everything RunMigrationImport needs. It is the
// adoption of an Unraid array (doc 05 §4 steps 14-16): the data disks are
// mounted read-only as they are and unioned at /mnt/user read-only, and the
// former parity and cache disks are recorded and left untouched. Nothing here
// writes a byte to a source disk: there is no format, no repair, and no
// read-write mount.
type MigrationImportDeps struct {
	// Plan resolves the confirmed mapping against the migration session's
	// report and a fresh disk inventory (migrate.Service.PlanImport).
	Plan func(ctx context.Context, assignments []disk.AdoptionAssignment) (disk.AdoptionPlan, error)
	// Runner runs the read-only filesystem checks and confirms each mount from
	// the kernel's mount table.
	Runner    disk.Runner
	Store     *store.ArrayStore
	Generator *config.Generator
	// Mounter brings the read-only disk units up and down.
	Mounter disk.UnitMounter
	// ArrayReady rebuilds the daemon's array sequence from the stored
	// topology, the hook the disk-topology jobs call.
	ArrayReady func(ctx context.Context) error
	// Array returns the daemon's current array sequence, which mounts and
	// unmounts the catch-all pool.
	Array func() *ArraySequence
	// Seed creates the shares and accounts of the Unraid configuration once the
	// disks are adopted and the pool is mounted (doc 05 §4 steps 3, 4 and 15),
	// writing to no adopted disk, and reports what it created to out. It is
	// all-or-nothing: an error means it left nothing of itself behind.
	Seed func(ctx context.Context, out io.Writer) error
	// InvalidateVerify forgets the migration's verify result
	// (migrate.Service.InvalidateVerify). The job calls it before it changes
	// anything, so a pass recorded before this run, which says nothing about what
	// is mounted after it, never opens the point of no return.
	InvalidateVerify func(ctx context.Context) error
	// Now, when set, stamps generated-file headers; nil uses time.Now.
	Now func() time.Time
	// IsMounted, when set, answers whether where is a mountpoint for the undo
	// of a failed adoption; nil asks the kernel's mount table.
	IsMounted func(where string) (bool, error)
}

// mounted reports whether where is a mountpoint. A path that does not exist is
// not one; any other failure is returned, never read as "not mounted".
func (d MigrationImportDeps) mounted(where string) (bool, error) {
	check := disk.IsMountpoint
	if d.IsMounted != nil {
		check = d.IsMounted
	}
	m, err := check(where)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return m, err
}

func (d MigrationImportDeps) complete() bool {
	return d.Plan != nil && d.Runner != nil && d.Store != nil && d.Generator != nil && d.Mounter != nil && d.ArrayReady != nil && d.Array != nil && d.Seed != nil && d.InvalidateVerify != nil
}

// RunMigrationImport is the RunFunc hoservad registers for TypeMigrationImport.
//
// It resolves the confirmed mapping again from a fresh inventory and refuses,
// before anything is written, unless that is the plan the request confirmed
// and every data disk passes its read-only filesystem check. Only then does it
// record the pending array in SQLite, write the read-only disk units and the
// read-only catch-all unit (no snapraid.conf), mount each disk and confirm
// from the mount table that it is the confirmed device, read-only, and mount
// the pool. A failure after the array is recorded undoes all of it: the pool
// and the disks are unmounted first, and only when they are is the record
// deleted, so the record never claims disks that are not mounted and a disk
// that cannot be released is reported, not forgotten.
//
// A second run with the same plan after one that stopped part-way (a crash,
// a failed rebuild) applies the recorded array again and undoes nothing; a run
// with another plan refuses with store.ErrArrayExists.
func RunMigrationImport(d MigrationImportDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeMigrationImportParams(rc.Params())
		if err != nil {
			return err
		}
		if !d.complete() {
			return errors.New("job: migration_import is missing its dependencies")
		}
		out := rc.Output()
		fresh, err := d.Plan(ctx, p.Assignments)
		if err != nil {
			return fmt.Errorf("resolving the confirmed disk-role mapping: %w", err)
		}
		if err := p.Plan.Matches(fresh); err != nil {
			return err
		}
		if err := d.InvalidateVerify(ctx); err != nil {
			return fmt.Errorf("forgetting the verify result before the import changes the pool: %w", err)
		}

		exists, err := d.Store.Exists(ctx)
		if err != nil {
			return err
		}
		if exists {
			return d.reapply(ctx, out, fresh)
		}

		_, _ = fmt.Fprintf(out, "checking %d data disk(s) read-only before anything is mounted\n", len(fresh.Data))
		if err := fresh.CheckData(ctx, d.Runner); err != nil {
			return err
		}
		created := d.now()
		disks, recorded := pendingRows(fresh)
		if err := d.Store.PutPendingArray(ctx, store.ArraySettings{
			CreatePolicy: string(pool.DefaultCreatePolicy),
			MinFreeSpace: pool.DefaultOptions().MinFreeSpace,
			CreatedAt:    created,
		}, disks, recorded); err != nil {
			return err
		}
		if err := d.apply(ctx, out, created); err != nil {
			return d.undo(ctx, out, err)
		}
		if err := d.Seed(ctx, out); err != nil {
			return d.undo(ctx, out, fmt.Errorf("seeding the shares and accounts: %w", err))
		}
		_, _ = fmt.Fprintf(out, "adopted %d data disk(s) read-only at /mnt/diskN and unioned them at %s; %d parity and %d cache disk(s) recorded and left untouched\n",
			len(fresh.Data), pool.CatchAllPath, len(fresh.Parity), cacheCount(fresh))
		return nil
	}
}

func (d MigrationImportDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func cacheCount(p disk.AdoptionPlan) int {
	if p.Cache == nil {
		return 0
	}
	return 1
}

// pendingRows are the rows PutPendingArray stores for plan: its data disks at
// /mnt/diskN in plan order, and the parity and cache disks as recorded
// identities.
func pendingRows(plan disk.AdoptionPlan) ([]store.ArrayDisk, []store.RecordedDisk) {
	units := plan.MountUnits()
	disks := make([]store.ArrayDisk, 0, len(plan.Data))
	for i, a := range plan.Data {
		disks = append(disks, store.ArrayDisk{
			Role:         store.ArrayRoleData,
			RoleIndex:    i + 1,
			Device:       a.Device,
			Filesystem:   string(a.Filesystem),
			FSUUID:       a.FSUUID,
			WWN:          a.WWN,
			Serial:       a.Serial,
			ByIDName:     a.ByIDName,
			WeakIdentity: a.WeakIdentity,
			Mountpoint:   units[i].Where,
			Size:         a.Size,
			SizeSet:      true,
			MountSource:  a.MountSource,
		})
	}
	var recorded []store.RecordedDisk
	record := func(role string, index int, r disk.RecordedDisk) {
		recorded = append(recorded, store.RecordedDisk{
			Role:         role,
			RoleIndex:    index,
			Device:       r.Device,
			Size:         r.Size,
			WWN:          r.WWN,
			Serial:       r.Serial,
			ByIDName:     r.ByIDName,
			WeakIdentity: r.WeakIdentity,
			PartUUID:     r.PartUUID,
		})
	}
	for i, r := range plan.Parity {
		record(store.ArrayRoleParity, i+1, r)
	}
	if plan.Cache != nil {
		record(store.ArrayRoleCache, 1, *plan.Cache)
	}
	return disks, recorded
}

// reapply is the retry of an adoption that is already recorded: it applies the
// stored array again when plan is the one stored, and refuses otherwise.
func (d MigrationImportDeps) reapply(ctx context.Context, out io.Writer, plan disk.AdoptionPlan) error {
	settings, disks, err := d.Store.GetArray(ctx)
	if err != nil {
		return err
	}
	if !settings.MigrationPending {
		return store.ErrArrayExists
	}
	recorded, err := d.Store.RecordedDisks(ctx)
	if err != nil {
		return err
	}
	wantDisks, wantRecorded := pendingRows(plan)
	if !samePendingRows(disks, recorded, wantDisks, wantRecorded) {
		return store.ErrArrayExists
	}
	_, _ = fmt.Fprintln(out, "this adoption is already recorded: applying it again")
	if err := d.apply(ctx, out, settings.CreatedAt); err != nil {
		return err
	}
	if err := d.Seed(ctx, out); err != nil {
		return fmt.Errorf("seeding the shares and accounts: %w", err)
	}
	return nil
}

func samePendingRows(disks []store.ArrayDisk, recorded []store.RecordedDisk, wantDisks []store.ArrayDisk, wantRecorded []store.RecordedDisk) bool {
	if len(disks) != len(wantDisks) || len(recorded) != len(wantRecorded) {
		return false
	}
	for i, d := range disks {
		w := wantDisks[i]
		if d.Role != w.Role || d.RoleIndex != w.RoleIndex || d.Mountpoint != w.Mountpoint || d.Filesystem != w.Filesystem ||
			!strings.EqualFold(d.FSUUID, w.FSUUID) || d.WWN != w.WWN || d.Serial != w.Serial || d.ByIDName != w.ByIDName || d.MountSource != w.MountSource {
			return false
		}
	}
	for i, r := range recorded {
		w := wantRecorded[i]
		if r.Role != w.Role || r.RoleIndex != w.RoleIndex || r.WWN != w.WWN || r.Serial != w.Serial || r.ByIDName != w.ByIDName || r.PartUUID != w.PartUUID {
			return false
		}
	}
	return true
}

// apply makes the stored pending array live: units, disk mounts, the confirmed
// mounts, the rebuilt sequence and the read-only pool.
func (d MigrationImportDeps) apply(ctx context.Context, out io.Writer, created time.Time) error {
	if err := applyArrayFromStore(ctx, d.Store, d.Generator, d.Mounter, created); err != nil {
		return err
	}
	settings, disks, err := d.Store.GetArray(ctx)
	if err != nil {
		return err
	}
	units, err := ArrayMountUnits(settings, disks)
	if err != nil {
		return err
	}
	for _, u := range units {
		if err := confirmAdoptedMount(ctx, d.Runner, u); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "%s: %s mounted read-only\n", u.Where, u.UUID)
	}
	if err := d.ArrayReady(ctx); err != nil {
		return err
	}
	seq := d.Array()
	if seq == nil || seq.CatchAll == nil {
		return errors.New("the array sequence has no catch-all pool mount after the import")
	}
	if err := seq.RefreshLive(ctx, true); err != nil {
		return err
	}
	if err := disk.ConfirmMountedReadOnly(ctx, d.Runner, seq.CatchAll.Where()); err != nil {
		return fmt.Errorf("the pool at %s: %w", seq.CatchAll.Where(), err)
	}
	return nil
}

// confirmAdoptedMount shows from the kernel's mount table that u is mounted
// where it should be: the filesystem the plan named, from the device the plan
// bound it to, read-only. A mount is not trusted for having started.
func confirmAdoptedMount(ctx context.Context, r disk.Runner, u disk.MountUnit) error {
	if err := disk.ConfirmMountedUUID(ctx, r, u.Where, u.UUID); err != nil {
		return err
	}
	if u.What != "" {
		if err := disk.ConfirmMountedSource(ctx, r, u.Where, u.What); err != nil {
			return err
		}
	}
	return disk.ConfirmMountedReadOnly(ctx, r, u.Where)
}

// undo reverses a failed adoption this run recorded and returns cause with what
// the undo did. The pool and the disks are unmounted first; the record, the
// generated units and the sequence follow only when every mount is gone.
func (d MigrationImportDeps) undo(ctx context.Context, out io.Writer, cause error) error {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrationImportUndoTimeout)
	defer cancel()
	_, _ = fmt.Fprintf(out, "the adoption failed (%v): undoing it\n", cause)

	var errs []error
	if seq := d.Array(); seq != nil && seq.CatchAll != nil {
		where := seq.CatchAll.Where()
		if mounted, err := d.mounted(where); err != nil {
			errs = append(errs, fmt.Errorf("checking whether %s is mounted: %w", where, err))
		} else if mounted {
			if err := seq.CatchAll.Unmount(uctx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	settings, disks, err := d.Store.GetArray(uctx)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("undoing the adoption: reading it back: %w", err))
	}
	units, err := ArrayMountUnits(settings, disks)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("undoing the adoption: %w", err))
	}
	for _, u := range units {
		mounted, err := d.mounted(u.Where)
		if err != nil {
			errs = append(errs, fmt.Errorf("checking whether %s is mounted: %w", u.Where, err))
			continue
		}
		if !mounted {
			continue
		}
		if err := d.Mounter.Unmount(uctx, u); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(append([]error{cause, errors.New("the adoption could not be undone: a mount is still up, so the record of it was kept")}, errs...)...)
	}
	if _, err := d.Store.DeletePendingArray(uctx); err != nil {
		return errors.Join(cause, fmt.Errorf("undoing the adoption: deleting its record: %w", err))
	}
	for _, u := range units {
		if err := d.Generator.RemoveDiskMount(uctx, u.Where); err != nil {
			errs = append(errs, err)
		}
	}
	// The catch-all's unit is generated like a disk's, at the pool's own path.
	if err := d.Generator.RemoveDiskMount(uctx, pool.CatchAllPath); err != nil {
		errs = append(errs, err)
	}
	if err := d.ArrayReady(uctx); err != nil {
		errs = append(errs, fmt.Errorf("rebuilding the array sequence: %w", err))
	}
	if len(errs) > 0 {
		return errors.Join(append([]error{cause, errors.New("the adoption was undone but its leftovers could not all be removed")}, errs...)...)
	}
	return cause
}
