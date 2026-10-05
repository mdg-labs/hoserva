package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

var (
	// ErrMigrationParityNotPending is returned when the parity initialisation is
	// asked for and no Unraid migration is waiting for its point of no return or
	// part-way through it.
	ErrMigrationParityNotPending = errors.New("there is no Unraid migration waiting for its point of no return")
	// ErrMigrationParityChanged is returned when the disks of the adoption are not
	// what the migration recorded: nothing is formatted.
	ErrMigrationParityChanged = errors.New("the disks are not the ones the adoption recorded")
)

// migrationParityRestoreTimeout bounds the restoring of the read-only adoption
// after a failure before the first disk was formatted, which runs without the
// job's own cancellation.
const migrationParityRestoreTimeout = 2 * time.Minute

// MigrationParityDeps is everything RunMigrationParity needs. It is the point of
// no return of an Unraid migration (doc 05 §4 step 17): the first step that
// writes to a disk of the old array. It formats the former parity disks and the
// cache, records them as the array's own, mounts the data disks read-write,
// generates snapraid.conf, applies what the import deferred, and queues the
// initial sync.
type MigrationParityDeps struct {
	// Plan is the gate and the plan together (migrate.Service.PlanParityInit): it
	// refuses unless the latest verify of the adopted array passed and no import
	// has run since, and resolves the recorded parity, cache and data disks again
	// from a fresh inventory.
	Plan func(ctx context.Context) (disk.AdoptionPlan, error)
	// Provider lists the disks and formats them (disk.FormatParityInit). Probe
	// confirms a spare boot-disk partition blank; nil probes with blkid through
	// Runner.
	Provider disk.Provider
	Probe    disk.BlankProber
	Runner   disk.Runner
	Store    *store.ArrayStore
	// Generator and Mounter write and mount the array's units.
	Generator *config.Generator
	Mounter   disk.UnitMounter
	// ArrayReady is the disk-topology jobs' hook: it regenerates the pool's mounts
	// and smb.conf from the share rows, rebuilds the array sequence, wires the
	// parity engine for the snapraid.conf just written (so sync, scrub and fix are
	// registered without a restart) and mounts the shares.
	ArrayReady func(ctx context.Context) error
	// Array returns the daemon's current array sequence.
	Array func() *ArraySequence
	// Shares applies the cache modes and share directory modes the import deferred
	// (share.Service.CompleteMigration), once the disks are mounted read-write.
	Shares func(ctx context.Context, out io.Writer) error
	// QueueSync queues the initial sync through the scheduler, as any sync is
	// queued: its run goes through the threshold guard, which no argument skips.
	QueueSync func(ctx context.Context) (jobID string, err error)
	// IsMounted, when set, answers whether where is a mountpoint; nil asks the
	// kernel's mount table.
	IsMounted func(where string) (bool, error)
}

func (d MigrationParityDeps) complete() bool {
	return d.Plan != nil && d.Provider != nil && d.Runner != nil && d.Store != nil && d.Generator != nil && d.Mounter != nil &&
		d.ArrayReady != nil && d.Array != nil && d.Shares != nil && d.QueueSync != nil
}

func (d MigrationParityDeps) mounted(where string) (bool, error) {
	return d.importDeps().mounted(where)
}

// importDeps is the adoption's own apply, which brings the pending array back
// read-only after a failure before anything was formatted.
func (d MigrationParityDeps) importDeps() MigrationImportDeps {
	return MigrationImportDeps{Runner: d.Runner, Store: d.Store, Generator: d.Generator, Mounter: d.Mounter, ArrayReady: d.ArrayReady, Array: d.Array, IsMounted: d.IsMounted}
}

// RunMigrationParity is the RunFunc hoservad registers for TypeMigrationParity.
//
// While the migration is pending it resolves the recorded disks again from a
// fresh inventory (Plan, which also refuses without a passing verify), refuses
// unless the typed confirmation is the one that plan computes and the disks are
// the ones the adoption recorded, then stops what holds the read-only mounts and
// unmounts them. Only then does it format, in one guarded step that resolves
// every target before the first mkfs (disk.FormatParityInit). Everything before
// that is undone by remounting the adoption read-only, so a failure before the
// first format leaves the pending state as it was; a failure while formatting
// does too, with the disks mounted read-only again, and a retry formats them
// again (nothing is on them) and never a data disk, which is not in the plan.
//
// After the formatting the parity and cache are recorded as array disks and the
// migration stops being pending in one write (store.ArrayStore.RecordParityInit),
// which is the one point after which "migration complete" could be read: it is
// only written once every disk is formatted. From there the job is finished by
// running it again (with disk.ParityInitFinishConfirmation), which formats
// nothing: it mounts the data disks read-write, generates snapraid.conf, applies
// the shares' deferred cache modes and directory modes, wires the parity engine,
// drops the record of the former disks (store.ArrayStore.FinishMigration) and
// queues the initial sync.
func RunMigrationParity(d MigrationParityDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeMigrationParityParams(rc.Params())
		if err != nil {
			return err
		}
		if !d.complete() {
			return errors.New("job: migration_parity is missing its dependencies")
		}
		out := rc.Output()
		pending, err := d.Store.MigrationPending(ctx)
		if err != nil {
			return err
		}
		finishing, err := d.Store.MigrationFinishing(ctx)
		if err != nil {
			return err
		}
		switch {
		case pending:
			if err := d.crossPointOfNoReturn(ctx, out, p.Confirmation); err != nil {
				return err
			}
		case finishing:
			if p.Confirmation != disk.ParityInitFinishConfirmation {
				return disk.ErrConfirmationMismatch
			}
			_, _ = fmt.Fprintln(out, "the parity and cache disks are already formatted and recorded: finishing the initialisation")
			if err := d.release(ctx, out); err != nil {
				return err
			}
		default:
			return ErrMigrationParityNotPending
		}
		return d.finish(ctx, out)
	}
}

// crossPointOfNoReturn is everything up to and including the record of the
// formatted disks: the part that is undone, as far as it can be, by remounting
// the adoption read-only.
func (d MigrationParityDeps) crossPointOfNoReturn(ctx context.Context, out io.Writer, confirmation string) error {
	fresh, err := d.Plan(ctx)
	if err != nil {
		return err
	}
	_, disks, err := d.Store.GetArray(ctx)
	if err != nil {
		return err
	}
	recorded, err := d.Store.RecordedDisks(ctx)
	if err != nil {
		return err
	}
	if err := sameAdoption(fresh, disks, recorded); err != nil {
		return err
	}
	if confirmation != fresh.ParityInitConfirmation() {
		return disk.ErrConfirmationMismatch
	}
	rows := parityInitRows(fresh)
	// snapraid.conf is rendered from these rows once they are recorded: a layout
	// that cannot be rendered (content files that cannot be placed on enough
	// distinct devices, Q18) is refused now, before anything is erased.
	if _, err := layoutFromStore(append(append([]store.ArrayDisk{}, disks...), rows...)).Render(); err != nil {
		return fmt.Errorf("the array cannot be configured for SnapRAID: %w", err)
	}

	_, _ = fmt.Fprintf(out, "point of no return: %s\n", confirmation)
	if err := d.release(ctx, out); err != nil {
		return d.restoreReadOnly(ctx, out, err)
	}
	probe := d.Probe
	if probe == nil {
		probe = disk.LinuxBlankProber{Exec: d.Runner}
	}
	if err := disk.FormatParityInit(ctx, d.Provider, probe, fresh, confirmation); err != nil {
		return d.restoreReadOnly(ctx, out, fmt.Errorf("formatting the former parity and cache disks: %w", err))
	}
	_, _ = fmt.Fprintln(out, "the former parity disk(s) and the cache are formatted XFS")
	for i := range rows {
		uuid, err := d.formattedUUID(ctx, rows[i])
		if err != nil {
			return d.restoreReadOnly(ctx, out, err)
		}
		rows[i].FSUUID = uuid
	}
	if err := d.Store.RecordParityInit(ctx, rows); err != nil {
		return d.restoreReadOnly(ctx, out, fmt.Errorf("recording the formatted disks: %w", err))
	}
	return nil
}

// sameAdoption refuses (ErrMigrationParityChanged) unless the disks fresh
// resolves are exactly those the adoption recorded: the data disks, and the
// former parity and cache disks by identity and size.
func sameAdoption(fresh disk.AdoptionPlan, disks []store.ArrayDisk, recorded []store.RecordedDisk) error {
	wantDisks, wantRecorded := pendingRows(fresh)
	if !samePendingRows(disks, recorded, wantDisks, wantRecorded) {
		return fmt.Errorf("%w: a disk was swapped, repartitioned or replaced since the import", ErrMigrationParityChanged)
	}
	for i, r := range recorded {
		if r.Size != wantRecorded[i].Size {
			return fmt.Errorf("%w: the size of %s changed since the import", ErrMigrationParityChanged, r.Device)
		}
	}
	return nil
}

// parityInitRows are the array disks the point of no return records: the former
// parity disks at /mnt/parityN and the cache at /mnt/cache. Their filesystem
// UUIDs are those of the filesystems formatted, read back after the format.
func parityInitRows(plan disk.AdoptionPlan) []store.ArrayDisk {
	row := func(role string, index int, mountpoint string, r disk.RecordedDisk) store.ArrayDisk {
		return store.ArrayDisk{
			Role:         role,
			RoleIndex:    index,
			Device:       r.Device,
			Filesystem:   string(disk.XFS),
			WWN:          r.WWN,
			Serial:       r.Serial,
			ByIDName:     r.ByIDName,
			WeakIdentity: r.WeakIdentity,
			Mountpoint:   mountpoint,
			Size:         r.Size,
			SizeSet:      true,
		}
	}
	var rows []store.ArrayDisk
	for i, r := range plan.Parity {
		rows = append(rows, row(store.ArrayRoleParity, i+1, fmt.Sprintf("/mnt/parity%d", i+1), r))
	}
	if plan.Cache != nil {
		rows = append(rows, row(store.ArrayRoleCache, 1, "/mnt/cache", *plan.Cache))
	}
	return rows
}

// formattedUUID reads the UUID of the filesystem just made on a parity or cache
// disk by a direct probe of the device it was formatted through.
func (d MigrationParityDeps) formattedUUID(ctx context.Context, row store.ArrayDisk) (string, error) {
	dev := row.Device
	if p := (disk.Identity{ByIDName: row.ByIDName}).IdentityPath(); p != "" {
		dev = p
	}
	uuid, err := disk.ProbedUUID(ctx, d.Runner, dev)
	if err != nil {
		return "", fmt.Errorf("reading the filesystem formatted on %s: %w", row.Device, err)
	}
	return uuid, nil
}

// release stops what holds the array's mounts and unmounts the pool and the
// disks, in doc 02 §4's order. Whatever is not mounted is left alone, so it is
// safe to run again after a failure part-way through.
func (d MigrationParityDeps) release(ctx context.Context, out io.Writer) error {
	seq := d.Array()
	if seq == nil {
		return errors.New("the daemon has no array sequence to release the read-only mounts from")
	}
	settings, disks, err := d.Store.GetArray(ctx)
	if err != nil {
		return err
	}
	units, err := ArrayMountUnits(settings, disks)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "stopping what holds the pool and unmounting it and the disks")
	for _, svc := range seq.Services {
		if err := svc.Stop(ctx); err != nil {
			return fmt.Errorf("stopping %s: %w", svc.Name(), err)
		}
	}
	if seq.PoolWriteGate != nil {
		if err := seq.PoolWriteGate.Close(ctx); err != nil {
			return fmt.Errorf("closing the pool destination to config backups: %w", err)
		}
	}
	for _, m := range seq.ShareMounts {
		if err := d.unmountMount(ctx, m); err != nil {
			return err
		}
	}
	if seq.CatchAll != nil {
		if err := d.unmountMount(ctx, seq.CatchAll); err != nil {
			return err
		}
	}
	for _, u := range units {
		mounted, err := d.mounted(u.Where)
		if err != nil {
			return fmt.Errorf("checking whether %s is mounted: %w", u.Where, err)
		}
		if !mounted {
			continue
		}
		if err := d.Mounter.Unmount(ctx, u); err != nil {
			return err
		}
	}
	return nil
}

func (d MigrationParityDeps) unmountMount(ctx context.Context, m ArrayMount) error {
	mounted, err := d.mounted(m.Where())
	if err != nil {
		return fmt.Errorf("checking whether %s is mounted: %w", m.Where(), err)
	}
	if !mounted {
		return nil
	}
	return m.Unmount(ctx)
}

// restoreReadOnly brings the pending adoption back, read-only, after a failure
// before the formatted disks were recorded, starts what release stopped, and
// returns cause with whatever the restoring could not do. It runs without the
// job's cancellation: a cancelled job must still leave the disks mounted.
func (d MigrationParityDeps) restoreReadOnly(ctx context.Context, out io.Writer, cause error) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrationParityRestoreTimeout)
	defer cancel()
	_, _ = fmt.Fprintf(out, "the point of no return failed (%v): mounting the adopted disks read-only again\n", cause)
	settings, _, err := d.Store.GetArray(rctx)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("remounting the adoption read-only: reading it back: %w", err))
	}
	if !settings.MigrationPending {
		return cause
	}
	imp := d.importDeps()
	if err := imp.apply(rctx, out, settings.CreatedAt); err != nil {
		return errors.Join(cause, fmt.Errorf("remounting the adoption read-only: %w", err))
	}
	if err := d.startServices(rctx); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// startServices starts what release stopped, in reverse stop order, and opens
// the pool destination to config backups again. It uses the sequence the daemon
// holds now, which ArrayReady rebuilt from the record.
func (d MigrationParityDeps) startServices(ctx context.Context) error {
	seq := d.Array()
	if seq == nil {
		return nil
	}
	if seq.PoolWriteGate != nil {
		seq.PoolWriteGate.Open()
	}
	var errs []error
	for i := len(seq.Services) - 1; i >= 0; i-- {
		if err := seq.Services[i].Start(ctx); err != nil {
			errs = append(errs, fmt.Errorf("starting %s: %w", seq.Services[i].Name(), err))
		}
	}
	return errors.Join(errs...)
}

// finish is everything after the formatted disks are recorded, and is the whole
// of a run that finishes an initialisation that stopped there. Each step is safe
// to run again.
func (d MigrationParityDeps) finish(ctx context.Context, out io.Writer) error {
	settings, disks, err := d.Store.GetArray(ctx)
	if err != nil {
		return err
	}
	units, err := ArrayMountUnits(settings, disks)
	if err != nil {
		return err
	}
	if err := applyArrayFromStore(ctx, d.Store, d.Generator, d.Mounter, settings.CreatedAt); err != nil {
		return fmt.Errorf("mounting the disks read-write and generating snapraid.conf: %w", err)
	}
	for _, u := range units {
		if err := d.confirmWritableMount(ctx, u); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "%s: %s mounted read-write\n", u.Where, u.UUID)
	}
	_, _ = fmt.Fprintln(out, "snapraid.conf generated")

	if err := d.Shares(ctx, out); err != nil {
		return fmt.Errorf("applying the shares' cache modes and directory modes: %w", err)
	}
	if err := d.ArrayReady(ctx); err != nil {
		return fmt.Errorf("applying the new topology: %w", err)
	}
	seq := d.Array()
	if seq == nil || seq.CatchAll == nil {
		return errors.New("the array sequence has no catch-all pool mount after the point of no return")
	}
	if err := seq.RefreshLive(ctx, true); err != nil {
		return err
	}
	if ro, err := disk.MountedReadOnly(ctx, d.Runner, seq.CatchAll.Where()); err != nil {
		return fmt.Errorf("the pool at %s: %w", seq.CatchAll.Where(), err)
	} else if ro {
		return fmt.Errorf("the pool at %s is still read-only", seq.CatchAll.Where())
	}
	if err := d.startServices(ctx); err != nil {
		return err
	}

	if err := d.Store.FinishMigration(ctx); err != nil {
		return err
	}
	id, err := d.QueueSync(ctx)
	if err != nil {
		return fmt.Errorf("the parity initialisation is finished, but the initial sync could not be queued: %w: the daemon queues it again when it next starts; start it yourself sooner, the array has no parity until it has run", err)
	}
	if err := d.Store.ClearInitialSyncOwed(context.WithoutCancel(ctx)); err != nil {
		_, _ = fmt.Fprintf(out, "the initial sync is queued (job %s), but recording that it is no longer owed failed (%v): the daemon queues another when it next starts\n", id, err)
		return nil
	}
	_, _ = fmt.Fprintf(out, "the initial sync is queued (job %s): until it completes the array has no redundancy\n", id)
	return nil
}

// confirmWritableMount shows from the kernel's mount table that u is mounted
// where it should be: the filesystem the record names, from the device the unit
// binds it to, and read-write.
func (d MigrationParityDeps) confirmWritableMount(ctx context.Context, u disk.MountUnit) error {
	if err := disk.ConfirmMountedUUID(ctx, d.Runner, u.Where, u.UUID); err != nil {
		return err
	}
	if u.What != "" {
		if err := disk.ConfirmMountedSource(ctx, d.Runner, u.Where, u.What); err != nil {
			return err
		}
	}
	ro, err := disk.MountedReadOnly(ctx, d.Runner, u.Where)
	if err != nil {
		return err
	}
	if ro {
		return fmt.Errorf("%s is still mounted read-only", u.Where)
	}
	return nil
}

// initialSyncHistoryLimit bounds how many succeeded parity jobs
// QueueOwedInitialSync reads to find a sync that already built parity.
const initialSyncHistoryLimit = 1000

// QueueOwedInitialSync is the start of the daemon finishing what step 17 owed:
// a migration that finished (store.ArrayStore.FinishMigration) records that the
// initial sync is owed until it is queued, so a stop between the two leaves the
// array without parity and with a record of it. When the sync is owed it is
// queued through the same scheduler path as the job's own (QueueInitialSync): an
// ordinary sync, admitted by the scheduler and run through the engine's
// threshold guard, which no argument skips. It returns the queued job's id, or
// "" when nothing was owed or a real sync has already succeeded, which builds
// the parity the initial sync was for and ends what is owed without another.
//
// A read of the record that fails is an error, never "nothing owed". A sync the
// scheduler refuses leaves what is owed in place for the next start; the owed
// record is cleared only after the scheduler has accepted the sync.
func QueueOwedInitialSync(ctx context.Context, arrays *store.ArrayStore, s *Scheduler) (string, error) {
	owed, err := arrays.InitialSyncOwed(ctx)
	if err != nil {
		return "", err
	}
	if !owed {
		return "", nil
	}
	built, err := s.realSyncSucceeded(ctx)
	if err != nil {
		return "", err
	}
	if built {
		if err := arrays.ClearInitialSyncOwed(ctx); err != nil {
			return "", err
		}
		return "", nil
	}
	id, err := QueueInitialSync(s)(ctx)
	if err != nil {
		return "", fmt.Errorf("queueing the initial sync the migration owes: %w", err)
	}
	if err := arrays.ClearInitialSyncOwed(ctx); err != nil {
		return id, fmt.Errorf("the initial sync is queued (job %s), but recording that it is no longer owed failed: %w", id, err)
	}
	return id, nil
}

// realSyncSucceeded reports whether a sync that is not a dry run has succeeded.
func (s *Scheduler) realSyncSucceeded(ctx context.Context) (bool, error) {
	class, status := ClassParity, StatusSucceeded
	jobs, err := s.store.List(ctx, ListFilter{Class: &class, Status: &status, Limit: initialSyncHistoryLimit})
	if err != nil {
		return false, fmt.Errorf("listing the succeeded syncs: %w", err)
	}
	for _, j := range jobs {
		if j.Type != TypeSync {
			continue
		}
		opts, err := SyncOptsFromParams(j.Params)
		if err != nil {
			return false, fmt.Errorf("reading the params of sync job %s: %w", j.ID, err)
		}
		if !opts.DryRun {
			return true, nil
		}
	}
	return false, nil
}

// QueueInitialSync is MigrationParityDeps.QueueSync over a scheduler: an
// ordinary sync job, in no way special. It is a real sync (not a dry run) with
// no confirmation of a guard block, so it runs through RunSync and the engine's
// threshold guard like every other sync, and a block of the guard stops it
// until a person decides.
func QueueInitialSync(s *Scheduler) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		params, err := json.Marshal(SyncParams{})
		if err != nil {
			return "", fmt.Errorf("encoding the initial sync's params: %w", err)
		}
		j, err := s.Submit(ctx, TypeSync, nil, params)
		if err != nil {
			return "", err
		}
		return j.ID, nil
	}
}
