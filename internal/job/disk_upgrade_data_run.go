package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
)

// This file implements doc 02 §4 "Larger data disk: the upgrade's state
// machine". Comments name the table's cells (E1–E8, UR1–UR9, Unwind,
// Establish, Release, startup recovery) rather than restating them.

// Error codes a data-disk upgrade records (doc 02 §4 E2, E3, E5).
const (
	codeDiskUpgradeCleanupFailed    = "disk_upgrade_cleanup_failed"
	codeDiskUpgradeMountUnconfirmed = "disk_upgrade_mount_unconfirmed"
	codeDiskUpgradeArrayNotSynced   = "disk_upgrade_array_not_synced"
	codeDiskUpgradeCopyMismatch     = "disk_upgrade_copy_mismatch"
	codeDiskUpgradeDiffNotClean     = "disk_upgrade_diff_not_clean"
)

var (
	// ErrDiskUpgradeDataArrayNotSynced is E2 None: the pre-format
	// `snapraid diff` reports something to sync. Nothing was formatted.
	ErrDiskUpgradeDataArrayNotSynced = errors.New("job: the array has unsynced changes — start the array, sync it, stop it again, then submit this upgrade again; nothing was formatted")
	// ErrDiskUpgradeDiffNotClean is E2 Diffing: `snapraid diff` against
	// the new disk shows removed or updated files. The upgrade was
	// unwound and the old disk is still the array's disk, unchanged.
	ErrDiskUpgradeDiffNotClean = errors.New("job: snapraid diff shows removed or updated files on the new disk — the old disk is still the array's disk, unchanged")
	// ErrDiskUpgradeCleanupFailed is Unwind's failure: something it must
	// unmount is still mounted, or a service above the disks would not
	// stop.
	ErrDiskUpgradeCleanupFailed = errors.New("job: the data-disk upgrade could not bring the array to fully stopped")
	// ErrDiskUpgradeMountUnconfirmed is E5's Establish failure on a
	// resume: a disk would not mount, or a path does not hold the
	// filesystem UUID the job fixed for it.
	ErrDiskUpgradeMountUnconfirmed = errors.New("job: the data-disk upgrade could not mount and confirm its disks")
)

// MountTable reads and changes mounts by path (doc 02 §4 UR4, UR5):
// disk.KernelMounts in production, a fake in tests.
type MountTable interface {
	// IsMounted reports whether path is a mountpoint in the kernel mount
	// table. A path that does not exist is not mounted.
	IsMounted(ctx context.Context, path string) (bool, error)
	// UnmountOnce makes one attempt to unmount path, whatever mounted it.
	UnmountOnce(ctx context.Context, path string) error
	// Mount mounts unit.UUID at unit.Where by filesystem UUID.
	Mount(ctx context.Context, unit disk.MountUnit) error
	// MountedUUID reads the filesystem UUID of the source mounted at path
	// from the kernel mount table.
	MountedUUID(ctx context.Context, path string) (string, error)
}

// unmountAttempts bounds UR5's "repeats until the path is no longer a
// mountpoint": enough for a stacked mount and a briefly busy one.
const (
	unmountAttempts   = 10
	unmountRetryDelay = 200 * time.Millisecond
)

// DiskUpgradeDataDeps is everything a data-disk upgrade needs: its run,
// its abort (Cancel of a queued or interrupted upgrade) and startup
// recovery share it, so all three unwind the same way.
type DiskUpgradeDataDeps struct {
	Provider  disk.Provider
	Runner    disk.Runner
	Store     *store.ArrayStore
	Generator *config.Generator
	Parity    parity.Engine
	Mounts    MountTable
	// Array returns the daemon's current array sequence: Unwind stops its
	// Services and unmounts its pool mounts.
	Array func() *ArraySequence
	// ArrayReady rebuilds the daemon's array sequence after Release.
	ArrayReady func(ctx context.Context) error
	// Now stamps generated-file headers; nil uses time.Now.
	Now func() time.Time
	// StagingPath overrides diskUpgradeStagingPath (G) in tests.
	StagingPath func(mountpoint string) string
	// Sleep waits between unmount attempts; nil uses time.Sleep.
	Sleep func(time.Duration)
}

func (d DiskUpgradeDataDeps) staging(mountpoint string) string {
	if d.StagingPath != nil {
		return d.StagingPath(mountpoint)
	}
	return diskUpgradeStagingPath(mountpoint)
}

func (d DiskUpgradeDataDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d DiskUpgradeDataDeps) validate() error {
	if d.Provider == nil || d.Runner == nil || d.Store == nil || d.Generator == nil || d.Parity == nil || d.Mounts == nil || d.Array == nil {
		return fmt.Errorf("job: disk_upgrade_data is missing dependencies")
	}
	return nil
}

// unmountPath is UR5: unmount path by path, whatever mounted it, until
// the kernel mount table no longer lists it. A path that is not mounted,
// or does not exist, is already done. An unreadable mount table is an
// error, never "unmounted".
func (d DiskUpgradeDataDeps) unmountPath(ctx context.Context, path string) error {
	sleep := d.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	var lastErr error
	for attempt := 0; attempt < unmountAttempts; attempt++ {
		mounted, err := d.Mounts.IsMounted(ctx, path)
		if err != nil {
			return fmt.Errorf("checking whether %s is mounted: %w", path, err)
		}
		if !mounted {
			return nil
		}
		if err := d.Mounts.UnmountOnce(ctx, path); err != nil {
			lastErr = err
			sleep(unmountRetryDelay)
		}
	}
	mounted, err := d.Mounts.IsMounted(ctx, path)
	if err != nil {
		return fmt.Errorf("checking whether %s is mounted: %w", path, err)
	}
	if !mounted {
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("%s is still mounted after %d unmount attempts: %w", path, unmountAttempts, lastErr)
	}
	return fmt.Errorf("%s is still mounted after %d unmount attempts", path, unmountAttempts)
}

// unwind is the Unwind procedure: services stop, then every pool mount,
// then every array disk mountpoint including S, then G, are unmounted by
// path, and it succeeds only once the mount table lists none of them. A
// service that will not stop, or a pool mount that stays up, stops it
// before any disk is unmounted. A failure is an interrupted outcome with
// disk_upgrade_cleanup_failed, naming the paths still mounted.
func (d DiskUpgradeDataDeps) unwind(ctx context.Context, mountpoint string) error {
	fail := func(paths []string, cause error) error {
		still := d.stillMounted(ctx, paths)
		return &OutcomeError{
			Status: StatusInterrupted,
			Code:   codeDiskUpgradeCleanupFailed,
			Err:    fmt.Errorf("%w: %v; still mounted: %s", ErrDiskUpgradeCleanupFailed, cause, describeMounted(still)),
		}
	}
	seq := d.Array()
	if seq == nil {
		return fail(nil, errors.New("the daemon has no array sequence"))
	}
	_, disks, err := d.Store.GetArray(ctx)
	if err != nil {
		return fail(nil, fmt.Errorf("reading the array's disks: %w", err))
	}
	var pool []string
	for _, m := range seq.ShareMounts {
		pool = append(pool, m.Where())
	}
	if seq.CatchAll != nil {
		pool = append(pool, seq.CatchAll.Where())
	}
	var diskPaths []string
	for _, dsk := range disks {
		diskPaths = append(diskPaths, dsk.Mountpoint)
	}
	staging := d.staging(mountpoint)
	all := append(append(append([]string{}, pool...), diskPaths...), staging)

	for _, svc := range seq.Services {
		if err := svc.Stop(ctx); err != nil {
			return fail(all, fmt.Errorf("stopping %s: %w", svc.Name(), err))
		}
	}
	for _, p := range pool {
		if err := d.unmountPath(ctx, p); err != nil {
			return fail(all, err)
		}
	}
	var errs []error
	for _, p := range append(diskPaths, staging) {
		if err := d.unmountPath(ctx, p); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fail(all, errors.Join(errs...))
	}
	if still := d.stillMounted(ctx, all); len(still) > 0 {
		return fail(all, errors.New("the mount table still lists them"))
	}
	return nil
}

// stillMounted returns every path in paths the mount table lists, and
// every path it could not check, marked as such.
func (d DiskUpgradeDataDeps) stillMounted(ctx context.Context, paths []string) []string {
	var out []string
	for _, p := range paths {
		mounted, err := d.Mounts.IsMounted(ctx, p)
		switch {
		case err != nil:
			out = append(out, p+" (unknown: "+err.Error()+")")
		case mounted:
			out = append(out, p)
		}
	}
	return out
}

func describeMounted(paths []string) string {
	if len(paths) == 0 {
		return "none"
	}
	return strings.Join(paths, ", ")
}

// establish is the Establish procedure: mount each unit by filesystem
// UUID, then confirm from the mount table that its path holds that UUID.
func (d DiskUpgradeDataDeps) establish(ctx context.Context, set []disk.MountUnit) error {
	for _, u := range set {
		if err := d.Mounts.Mount(ctx, u); err != nil {
			return fmt.Errorf("mounting %s at %s: %w", u.UUID, u.Where, err)
		}
		if err := d.confirmUUID(ctx, u.Where, u.UUID); err != nil {
			return err
		}
	}
	return nil
}

// confirmUUID is UR4's check: the source mounted at where carries uuid.
func (d DiskUpgradeDataDeps) confirmUUID(ctx context.Context, where, uuid string) error {
	got, err := d.Mounts.MountedUUID(ctx, where)
	if err != nil {
		return fmt.Errorf("confirming the filesystem mounted at %s: %w", where, err)
	}
	if got != uuid {
		return fmt.Errorf("%s holds filesystem UUID %s, want %s", where, got, uuid)
	}
	return nil
}

// upgradeMountSets builds Establish's sets for one upgrade: every other
// data and parity disk from SQLite, A at S from the job's parameters, and
// B at S or G from the checkpoint's UUID (UR4). Establish(New) never
// includes A.
type upgradeMountSets struct {
	others []disk.MountUnit
	old    disk.MountUnit
}

func (d DiskUpgradeDataDeps) mountSets(disks []store.ArrayDisk, params DiskUpgradeDataParams) (upgradeMountSets, error) {
	var sets upgradeMountSets
	var slot store.ArrayDisk
	found := false
	for _, dsk := range disks {
		if dsk.Mountpoint == params.Mountpoint {
			slot = dsk
			found = true
			continue
		}
		if dsk.Role != store.ArrayRoleData && dsk.Role != store.ArrayRoleParity {
			continue
		}
		if dsk.FSUUID == "" {
			return sets, fmt.Errorf("job: array disk %s has no filesystem UUID", dsk.Mountpoint)
		}
		sets.others = append(sets.others, disk.MountUnit{
			Where:       dsk.Mountpoint,
			UUID:        dsk.FSUUID,
			Filesystem:  disk.FilesystemType(dsk.Filesystem),
			Description: diskMountDescription(dsk),
		})
	}
	if !found || slot.Role != store.ArrayRoleData {
		return sets, fmt.Errorf("job: no data disk at %s", params.Mountpoint)
	}
	sets.old = disk.MountUnit{
		Where:       params.Mountpoint,
		UUID:        params.Old.FSUUID,
		Filesystem:  params.Old.Filesystem,
		Description: diskMountDescription(slot),
	}
	return sets, nil
}

func (s upgradeMountSets) oldSet() []disk.MountUnit {
	return append(append([]disk.MountUnit{}, s.others...), s.old)
}

func (s upgradeMountSets) copySet(staging, newUUID string, fs disk.FilesystemType) []disk.MountUnit {
	return append(s.oldSet(), disk.MountUnit{Where: staging, UUID: newUUID, Filesystem: fs, Description: "Hoserva data disk upgrade staging"})
}

func (s upgradeMountSets) newSet(newUUID string, fs disk.FilesystemType) []disk.MountUnit {
	return append(append([]disk.MountUnit{}, s.others...), disk.MountUnit{Where: s.old.Where, UUID: newUUID, Filesystem: fs, Description: s.old.Description})
}

// pathMounter is disk.RunDataDiskUpgrade's Mounter: it mounts by UUID and
// unmounts by path (UR5).
type pathMounter struct{ d DiskUpgradeDataDeps }

func (m pathMounter) Mount(ctx context.Context, unit disk.MountUnit) error {
	return m.d.Mounts.Mount(ctx, unit)
}

func (m pathMounter) Unmount(ctx context.Context, unit disk.MountUnit) error {
	return m.d.unmountPath(ctx, unit.Where)
}

// RunDiskUpgradeData is the RunFunc hoservad registers for
// TypeDiskUpgradeData. Every run starts with Unwind and ends with Unwind;
// a failed final Unwind records the job interrupted at its last saved
// checkpoint with disk_upgrade_cleanup_failed, whatever the run's own
// outcome was.
func RunDiskUpgradeData(d DiskUpgradeDataDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		if err := d.validate(); err != nil {
			return err
		}
		params, err := decodeDiskUpgradeDataParams(rc.Params())
		if err != nil {
			return err
		}
		cp, err := decodeDataDiskUpgradeCheckpoint(rc.InitialCheckpoint())
		if err != nil {
			return err
		}
		if err := d.unwind(ctx, params.Mountpoint); err != nil {
			return err
		}
		runErr := d.run(ctx, rc, params, cp)
		if err := d.unwind(context.WithoutCancel(ctx), params.Mountpoint); err != nil {
			if runErr != nil {
				_, _ = fmt.Fprintf(rc.Output(), "data disk upgrade: %v\n", runErr)
			}
			return err
		}
		return runErr
	}
}

func decodeDataDiskUpgradeCheckpoint(data []byte) (disk.DataDiskUpgradeCheckpoint, error) {
	var cp disk.DataDiskUpgradeCheckpoint
	if len(data) == 0 {
		return cp, nil
	}
	if err := json.Unmarshal(data, &cp); err != nil {
		return cp, fmt.Errorf("job: decoding data disk upgrade checkpoint: %w", err)
	}
	if cp.Phase == disk.DataDiskUpgradePhaseFormatting {
		cp.Phase = ""
	}
	return cp, nil
}

// run is everything between the first and the final Unwind: E1 from the
// state the checkpoint names, with E2's and E5's outcomes.
func (d DiskUpgradeDataDeps) run(ctx context.Context, rc *RunContext, params DiskUpgradeDataParams, cp disk.DataDiskUpgradeCheckpoint) error {
	_, disks, err := d.Store.GetArray(ctx)
	if err != nil {
		return d.outcomeAt(cp.Phase, fmt.Errorf("job: reading the array's disks: %w", err))
	}
	sets, err := d.mountSets(disks, params)
	if err != nil {
		return d.outcomeAt(cp.Phase, err)
	}
	staging := d.staging(params.Mountpoint)
	fs := params.Disk.Filesystem

	switch cp.Phase {
	case "":
		if err := d.confirmBeforeFormat(ctx, disks, params); err != nil {
			return err
		}
		if err := d.establish(ctx, sets.oldSet()); err != nil {
			return fmt.Errorf("job: mounting the array's disks before formatting: %w", err)
		}
		report, err := d.Parity.Diff(ctx)
		if err != nil {
			return fmt.Errorf("job: snapraid diff before formatting the new disk: %w", err)
		}
		if report.Added+report.Removed+report.Updated+report.Moved+report.Copied != 0 {
			return &OutcomeError{Status: StatusFailed, Code: codeDiskUpgradeArrayNotSynced, Err: fmt.Errorf("%w (snapraid diff: %d added, %d removed, %d updated, %d moved, %d copied)", ErrDiskUpgradeDataArrayNotSynced, report.Added, report.Removed, report.Updated, report.Moved, report.Copied)}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if stopRequested(rc) {
			return nil
		}
	case disk.DataDiskUpgradePhaseCopying, disk.DataDiskUpgradePhaseVerifying:
		if err := d.establish(ctx, sets.copySet(staging, cp.NewUUID, fs)); err != nil {
			return mountUnconfirmed(err)
		}
	case disk.DataDiskUpgradePhaseRemounting:
		if err := d.establish(ctx, sets.newSet(cp.NewUUID, fs)); err != nil {
			return mountUnconfirmed(err)
		}
		next := disk.DataDiskUpgradeCheckpoint{Phase: disk.DataDiskUpgradePhaseDiffing, NewUUID: cp.NewUUID}
		data, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("job: encoding data disk upgrade checkpoint: %w", err)
		}
		if err := rc.SaveCheckpoint(data); err != nil {
			return d.outcomeAt(cp.Phase, err)
		}
		cp = next
	case disk.DataDiskUpgradePhaseDiffing:
		if err := d.establish(ctx, sets.newSet(cp.NewUUID, fs)); err != nil {
			return mountUnconfirmed(err)
		}
	case disk.DataDiskUpgradePhaseReleasing:
	default:
		return fmt.Errorf("job: unknown data disk upgrade checkpoint phase %q", cp.Phase)
	}

	phase := cp.Phase
	var newUUID string
	var removed, updated int
	spec := disk.DataDiskUpgradeSpec{
		Old: sets.old,
		New: disk.DiskAddition{
			Device:       params.Disk.Device,
			Filesystem:   params.Disk.Filesystem,
			WWN:          params.Disk.WWN,
			Serial:       params.Disk.Serial,
			WeakIdentity: params.Disk.WeakIdentity,
			ByIDName:     params.Disk.ByIDName,
		},
		NewFilesystem: fs,
		Staging:       staging,
	}
	deps := disk.DataDiskUpgradeDeps{
		Provider: d.Provider,
		Runner:   d.Runner,
		Mounter:  pathMounter{d: d},
		Diff: func(ctx context.Context) (int, int, error) {
			report, err := d.Parity.Diff(ctx)
			if err != nil {
				return 0, 0, err
			}
			removed, updated = report.Removed, report.Updated
			return report.Removed, report.Updated, nil
		},
		Release: func(ctx context.Context) error {
			return d.release(ctx, rc, params, newUUID)
		},
		StagingMounted: func(path string) (bool, error) {
			return d.Mounts.IsMounted(ctx, path)
		},
		ConfirmMounted: d.confirmUUID,
	}
	hooks := disk.DataDiskUpgradeHooks{
		StopRequested: rc.StopRequested(),
		SaveCheckpoint: func(data []byte) error {
			var next disk.DataDiskUpgradeCheckpoint
			if err := json.Unmarshal(data, &next); err != nil {
				return fmt.Errorf("job: decoding data disk upgrade checkpoint: %w", err)
			}
			if err := rc.SaveCheckpoint(data); err != nil {
				return err
			}
			phase = next.Phase
			newUUID = next.NewUUID
			return nil
		},
		SetProgress: rc.SetProgress,
		Log: func(format string, args ...any) {
			_, _ = fmt.Fprintf(rc.Output(), format+"\n", args...)
		},
	}
	newUUID = cp.NewUUID

	var cpBytes []byte
	if cp.Phase != "" {
		if cpBytes, err = json.Marshal(cp); err != nil {
			return fmt.Errorf("job: encoding data disk upgrade checkpoint: %w", err)
		}
	}
	result, err := disk.RunDataDiskUpgrade(ctx, spec, deps, hooks, cpBytes)
	if err != nil {
		if phase == disk.DataDiskUpgradePhaseVerifying && errors.Is(err, disk.ErrDataDiskUpgradeMismatch) {
			return &OutcomeError{Status: StatusFailed, Code: codeDiskUpgradeCopyMismatch, Err: err}
		}
		return d.outcomeAt(phase, err)
	}
	if result.Interrupted || result.Released {
		return nil
	}
	return &OutcomeError{Status: StatusFailed, Code: codeDiskUpgradeDiffNotClean, Err: fmt.Errorf("%w (%d removed, %d updated)", ErrDiskUpgradeDiffNotClean, removed, updated)}
}

// outcomeAt maps an error to E2's result for the state it happened in:
// before the copying checkpoint the job fails, since nothing of value is
// on the new disk yet; from copying on it is interrupted at that
// checkpoint, resumable (job_needs_retry).
func (d DiskUpgradeDataDeps) outcomeAt(phase disk.DataDiskUpgradePhase, err error) error {
	if phase == "" {
		return err
	}
	return fmt.Errorf("%w: %v", ErrJobNeedsRetry, err)
}

func mountUnconfirmed(err error) error {
	return &OutcomeError{Status: StatusInterrupted, Code: codeDiskUpgradeMountUnconfirmed, Err: fmt.Errorf("%w: %v", ErrDiskUpgradeMountUnconfirmed, err)}
}

func stopRequested(rc *RunContext) bool {
	select {
	case <-rc.StopRequested():
		return true
	default:
		return false
	}
}

// confirmBeforeFormat is E1 None's identity step: the typed confirmation
// still matches, SQLite still names A at the slot, A and B are the disks
// the plan was confirmed against by their by-id identities, and the Q20
// and topology checks still pass.
func (d DiskUpgradeDataDeps) confirmBeforeFormat(ctx context.Context, disks []store.ArrayDisk, params DiskUpgradeDataParams) error {
	if SingleDiskConfirmation(params.Disk) != params.Confirmation {
		return disk.ErrConfirmationMismatch
	}
	slot, ok := arrayDiskAtMountpoint(disks, params.Mountpoint)
	if !ok || slot.FSUUID != params.Old.FSUUID {
		return fmt.Errorf("job: SQLite no longer names the disk this upgrade was confirmed against at %s", params.Mountpoint)
	}
	listed, err := d.Provider.List(ctx)
	if err != nil {
		return fmt.Errorf("job: listing disks to confirm the old and new disks: %w", err)
	}
	if err := confirmByIDIdentity(listed, params.Old); err != nil {
		return fmt.Errorf("job: confirming the old disk: %w", err)
	}
	if err := confirmByIDIdentity(listed, params.Disk); err != nil {
		return fmt.Errorf("job: confirming the new disk: %w", err)
	}
	if err := refuseKnownIdentity([]store.ArrayDisk{slot}, params.Disk); err != nil {
		return fmt.Errorf("job: %s already occupies %s — nothing to upgrade: %w", params.Disk.Device, params.Mountpoint, err)
	}
	if disk.DataDiskUpgradeExceedsParity(params.Sizes[params.Disk.Device], parityDiskSizes(disks, params.Sizes)) {
		return ErrDataDiskUpgradeExceedsParity
	}
	return ValidateDiskReplacement(disks, params.Mountpoint, params.Disk, params.Sizes)
}

// confirmByIDIdentity refuses (ErrDiskIdentityDrifted) unless want.Device
// still carries want's by-id identity: WWN, serial, by-id name and weak
// flag. The filesystem UUID is not compared: a resume at none may find B
// already formatted by the run it resumes (doc 02 §4 E5 Interrupted at
// none).
func confirmByIDIdentity(listed []disk.Disk, want disk.AssignedDisk) error {
	if disk.IsLoopDevice(want.Device) {
		return nil
	}
	current, err := disk.LookupDisk(listed, want.Device)
	if err != nil {
		return fmt.Errorf("%w: %s is no longer listed", ErrDiskIdentityDrifted, want.Device)
	}
	if current.WWN != want.WWN || current.Serial != want.Serial || current.ByIDName != want.ByIDName || current.WeakIdentity != want.WeakIdentity {
		return fmt.Errorf("%w: %s", ErrDiskIdentityDrifted, want.Device)
	}
	return nil
}

// release is the Release procedure: one SQLite transaction naming B for
// the slot (skipped when SQLite already names B), then every generated
// file regenerated from SQLite, then the array sequence rebuilt. It needs
// no disk mounted and mounts none.
func (d DiskUpgradeDataDeps) release(ctx context.Context, rc *RunContext, params DiskUpgradeDataParams, newUUID string) error {
	if newUUID == "" {
		return errors.New("job: releasing without the new disk's filesystem UUID")
	}
	_, disks, err := d.Store.GetArray(ctx)
	if err != nil {
		return fmt.Errorf("job: reading the array before release: %w", err)
	}
	slot, ok := arrayDiskAtMountpoint(disks, params.Mountpoint)
	if !ok {
		return fmt.Errorf("job: no data disk at %s", params.Mountpoint)
	}
	if slot.FSUUID != newUUID {
		row := store.ArrayDisk{
			Device:       params.Disk.Device,
			Filesystem:   string(params.Disk.Filesystem),
			FSUUID:       newUUID,
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
			return fmt.Errorf("job: naming the new disk for %s: %w", params.Mountpoint, err)
		}
	}
	if err := regenerateArrayFromStore(ctx, d.Store, d.Generator, d.now()); err != nil {
		return fmt.Errorf("job: regenerating config after release: %w", err)
	}
	if d.ArrayReady != nil {
		if err := d.ArrayReady(ctx); err != nil {
			return fmt.Errorf("job: rebuilding the array sequence after release: %w", err)
		}
	}
	d.noticeIfNewDiskMissing(ctx, rc, params)
	return nil
}

// noticeIfNewDiskMissing writes E5's result notice when B is not listed
// at release: the array starts degraded, B is replaced like any failed
// disk, and A still holds a complete copy of the slot until then.
func (d DiskUpgradeDataDeps) noticeIfNewDiskMissing(ctx context.Context, rc *RunContext, params DiskUpgradeDataParams) {
	listed, err := d.Provider.List(ctx)
	if err == nil {
		if _, err = disk.LookupDisk(listed, params.Disk.Device); err == nil {
			return
		}
	}
	_, _ = fmt.Fprintf(rc.Output(), "data disk upgrade: the new disk %s was not found at release (%v). %s now names it, so the array starts degraded and the new disk is replaced like any failed disk. The old disk %s still holds a complete copy of %s's files — do not reuse it until the replacement has been rebuilt and verified.\n",
		params.Disk.Device, err, params.Mountpoint, params.Old.Device, params.Mountpoint)
}

// AbortDiskUpgradeData is the AbortFunc Cancel runs for a queued or
// interrupted data-disk upgrade (doc 02 §4 E3): Unwind. Cancel records
// cancelled only once it succeeded.
func AbortDiskUpgradeData(d DiskUpgradeDataDeps) AbortFunc {
	return func(ctx context.Context, _ string, rawParams []byte) error {
		params, err := decodeDiskUpgradeDataParams(rawParams)
		if err != nil {
			return fmt.Errorf("job: decoding disk_upgrade_data params to abort it: %w", err)
		}
		return d.unwind(ctx, params.Mountpoint)
	}
}

// RecoverDiskUpgradeData is startup recovery (doc 02 §4 E4, UR1, UR8).
// hoservad calls it once, after Scheduler.RecoverFromRestart and before
// it serves any request that submits or resumes a job. If a data-disk
// upgrade is pending it enters maintenance mode, then runs Unwind for
// each. An Unwind failure is recorded on its job as
// disk_upgrade_cleanup_failed and never stops the daemon. An error is
// returned only when the pending upgrades cannot be listed; maintenance
// mode is entered then too.
func RecoverDiskUpgradeData(ctx context.Context, s *Scheduler, d DiskUpgradeDataDeps) error {
	pending, err := s.store.ListPending(ctx, TypeDiskUpgradeData)
	if err != nil {
		if merr := s.EnterMaintenance(ctx); merr != nil {
			return errors.Join(err, merr)
		}
		return fmt.Errorf("job: listing pending data-disk upgrades: %w", err)
	}
	if len(pending) == 0 {
		return nil
	}
	if err := s.EnterMaintenance(ctx); err != nil {
		return err
	}
	var errs []error
	for _, j := range pending {
		var uerr error
		if verr := d.validate(); verr != nil {
			uerr = verr
		} else if params, perr := decodeDiskUpgradeDataParams(j.Params); perr != nil {
			uerr = perr
		} else {
			uerr = d.unwind(ctx, params.Mountpoint)
		}
		if uerr == nil {
			continue
		}
		if err := s.store.UpdateStatus(ctx, j.ID, j.Status, j.Progress, codeDiskUpgradeCleanupFailed, uerr.Error(), j.StartedAt, j.FinishedAt); err != nil {
			errs = append(errs, fmt.Errorf("job: recording the startup recovery failure of %s: %w", j.ID, err))
		}
	}
	return errors.Join(errs...)
}
