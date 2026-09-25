package job

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
)

// ErrDiskNotEvacuated refuses finishing the removal of a disk whose
// evacuation has not finished (doc 09 §4 steps 1-6, #274): only an
// "evacuated" disk, or one a previous finish already took further, may
// leave the pool.
var ErrDiskNotEvacuated = errors.New("job: the disk has not been evacuated")

// mountReader is the part of MountTable disk_remove reads: whether a
// path is mounted, and by which filesystem.
type mountReader interface {
	IsMounted(ctx context.Context, path string) (bool, error)
	MountedUUID(ctx context.Context, path string) (string, error)
}

// DiskRemoveDeps is what RunDiskRemove needs to finish removing an
// evacuated data disk (doc 09 §4 steps 7-9, #358).
type DiskRemoveDeps struct {
	Store     *store.ArrayStore
	Generator *config.Generator
	// Mounts reads the kernel mount table: the disk must be mounted by
	// its own filesystem for its emptiness to be checked at all.
	Mounts mountReader
	// Unmounter stops the disk's own mount unit (step 9) —
	// disk.SystemdMounter in production.
	Unmounter disk.UnitMounter
	Parity    parity.Engine
	// ShareNames lists every share, whose branch on the disk
	// cache.EvacuationPostCheck must find empty.
	ShareNames func(ctx context.Context) ([]string, error)
	// ArrayReady regenerates every pool mount from the store and applies
	// it to the running pool, returning a failed live update rather than
	// logging it — the same strict hook the evacuation uses (#359).
	ArrayReady func(ctx context.Context) error
	// Now, when set, stamps generated-file headers; nil uses time.Now.
	Now func() time.Time
}

// RunDiskRemove is the RunFunc hoservad registers for TypeDiskRemove:
// finishing an evacuated data disk's removal (doc 09 §4 steps 7-9).
//
// Before it changes anything it requires: the typed confirmation; a data
// disk at the slot whose removal state is "evacuated" or later; that the
// array without it still validates, with at least one data disk and
// room for every content-file copy (Q18); that the disk is mounted by its
// own filesystem; that cache.EvacuationPostCheck finds every share's
// branch on it empty; and that nothing but directories and SnapRAID's
// own content files is left anywhere else on it. Then, each step keyed
// on the persisted removal state (D4: state first), so a re-run carries
// on from the last one that finished (Q29: re-run, not resumed):
//
//   - Step 7: "unpooled" is persisted and ArrayReady takes the disk out
//     of every pool mount, live. A failed live update fails the job here.
//   - Step 8: the empty directories the evacuation left are removed
//     (rmdir only) — SnapRAID records directories too. With the disk
//     still listed in snapraid.conf, a sync through the threshold guard
//     exempts this disk alone (RemovingDisks), a fresh diff must show
//     SnapRAID tracking no file on it, and nothing but its content files
//     may be left on it. Only then is "unlisted" persisted and
//     snapraid.conf regenerated without its data line, and `snapraid
//     status` must accept the result — SnapRAID refuses every later
//     status, diff and sync if the line goes while it still records
//     anything on the disk. A guard trip or failed sync leaves the disk
//     "unpooled", still listed, and a re-run syncs again; once
//     "unlisted", a re-run never syncs.
//   - Step 9: the disk's own mount unit is stopped (unless it is already
//     unmounted), its unit file removed, the row deleted and ArrayReady
//     rebuilds the daemon's view. The filesystem is never wiped.
func RunDiskRemove(d DiskRemoveDeps) RunFunc {
	return func(ctx context.Context, rc *RunContext) error {
		p, err := decodeDiskRemoveParams(rc.Params())
		if err != nil {
			return err
		}
		if d.Store == nil || d.Generator == nil || d.Mounts == nil || d.Unmounter == nil || d.Parity == nil || d.ShareNames == nil || d.ArrayReady == nil {
			return fmt.Errorf("job: disk_remove is missing dependencies")
		}
		if rc.JobID() == "" {
			return fmt.Errorf("job: disk_remove: the run has no job id to hold the removal state")
		}
		return d.run(ctx, rc, p)
	}
}

func (d DiskRemoveDeps) run(ctx context.Context, rc *RunContext, p DiskRemoveParams) error {
	mountpoint := filepath.Clean(p.Mountpoint)
	logf := func(format string, args ...any) {
		_, _ = fmt.Fprintf(rc.Output(), format+"\n", args...)
	}

	if EvacuationConfirmation(p.Mountpoint) != p.Confirmation {
		return disk.ErrConfirmationMismatch
	}
	_, disks, err := d.Store.GetArray(ctx)
	if err != nil {
		return err
	}
	row, ok := dataDiskRow(disks, mountpoint)
	if !ok {
		return fmt.Errorf("%w: %s", store.ErrArrayDiskNotFound, p.Mountpoint)
	}
	state := row.RemovalState
	switch state {
	case store.RemovalStateEvacuated, store.RemovalStateUnpooled, store.RemovalStateUnlisted:
	default:
		return fmt.Errorf("%w: %s is %q — evacuate it first", ErrDiskNotEvacuated, row.Mountpoint, state)
	}
	var remaining []store.ArrayDisk
	for _, other := range disks {
		if other.Mountpoint != row.Mountpoint {
			remaining = append(remaining, other)
		}
	}
	if _, err := layoutFromStore(remaining).ContentPaths(); err != nil {
		return fmt.Errorf("job: disk_remove: the array without %s would not be valid: %w", row.Mountpoint, err)
	}
	mounted, err := mountedAs(ctx, d.Mounts, row.Mountpoint, row.FSUUID)
	if err != nil {
		return err
	}
	// Until the step-8 sync has recorded the disk empty, its emptiness
	// must be seen, not assumed: an unmounted slot shows the boot disk's
	// empty directory, and would pass any check.
	if !mounted && state != store.RemovalStateUnlisted {
		return fmt.Errorf("job: disk_remove: %s is not mounted, so it cannot be confirmed empty — start the array and finish the removal again", row.Mountpoint)
	}
	if mounted {
		if err := d.postCheck(ctx, row.Mountpoint); err != nil {
			return err
		}
		if err := diskLeftover(row.Mountpoint, true); err != nil {
			return err
		}
	}

	if state == store.RemovalStateEvacuated {
		if err := d.Store.AdvanceRemovalState(ctx, row.Mountpoint, store.RemovalStateEvacuated, store.RemovalStateUnpooled, rc.JobID()); err != nil {
			return fmt.Errorf("job: disk_remove: marking %s unpooled: %w", row.Mountpoint, err)
		}
		state = store.RemovalStateUnpooled
	}
	if state == store.RemovalStateUnpooled {
		logf("step 7: taking %s out of every pool mount", row.Mountpoint)
		if err := d.ArrayReady(ctx); err != nil {
			return fmt.Errorf("job: disk_remove: taking %s out of the running pool: %w", row.Mountpoint, err)
		}
		// Nothing places new files on the disk any more; check once more
		// right before parity records it empty.
		if err := d.postCheck(ctx, row.Mountpoint); err != nil {
			return err
		}
		if err := diskLeftover(row.Mountpoint, true); err != nil {
			return err
		}
		// SnapRAID records empty directories too, and refuses every later
		// run once a disk whose directories it still records leaves
		// snapraid.conf. The evacuation leaves each share's directory
		// tree behind, so it goes before the sync that records the disk
		// empty.
		if err := removeEmptyDirs(row.Mountpoint); err != nil {
			return err
		}
		logf("step 8: syncing parity with %s recorded empty, through the threshold guard", row.Mountpoint)
		ch, err := d.Parity.Sync(ctx, parity.SyncOpts{RemovingDisks: map[string]bool{mountpoint: true}})
		if err := drainProgress(ch, err); err != nil {
			return fmt.Errorf("job: disk_remove: the sync recording %s empty did not complete: %w", row.Mountpoint, err)
		}
		if err := d.confirmUntracked(ctx, mountpoint); err != nil {
			return err
		}
		if err := diskLeftover(row.Mountpoint, false); err != nil {
			return fmt.Errorf("%w — finish the removal again to record it empty", err)
		}
		if err := d.Store.AdvanceRemovalState(ctx, row.Mountpoint, store.RemovalStateUnpooled, store.RemovalStateUnlisted, rc.JobID()); err != nil {
			return fmt.Errorf("job: disk_remove: marking %s unlisted: %w", row.Mountpoint, err)
		}
	}

	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	logf("step 8: dropping %s from snapraid.conf", row.Mountpoint)
	if err := regenerateArrayFromStore(ctx, d.Store, d.Generator, now()); err != nil {
		return fmt.Errorf("job: disk_remove: regenerating the array's configuration without %s: %w", row.Mountpoint, err)
	}
	if _, err := d.Parity.Status(ctx); err != nil {
		return fmt.Errorf("job: disk_remove: SnapRAID does not accept the configuration without %s: %w", row.Mountpoint, err)
	}

	logf("step 9: unmounting %s", row.Mountpoint)
	if mounted {
		unit := disk.MountUnit{Where: row.Mountpoint, UUID: row.FSUUID, Filesystem: disk.FilesystemType(row.Filesystem)}
		if err := d.Unmounter.Unmount(ctx, unit); err != nil {
			return fmt.Errorf("job: disk_remove: unmounting %s: %w", row.Mountpoint, err)
		}
		still, err := d.Mounts.IsMounted(ctx, row.Mountpoint)
		if err != nil {
			return fmt.Errorf("job: disk_remove: confirming %s is unmounted: %w", row.Mountpoint, err)
		}
		if still {
			return fmt.Errorf("job: disk_remove: %s is still mounted after stopping its mount unit", row.Mountpoint)
		}
	}
	if err := d.Generator.RemoveDiskMount(ctx, row.Mountpoint); err != nil {
		return fmt.Errorf("job: disk_remove: removing %s's mount unit: %w", row.Mountpoint, err)
	}
	if err := d.Store.DeleteUnlistedDataDisk(ctx, row.Mountpoint); err != nil {
		return fmt.Errorf("job: disk_remove: deleting %s from the array: %w", row.Mountpoint, err)
	}
	if err := d.ArrayReady(ctx); err != nil {
		return fmt.Errorf("job: disk_remove: %s has left the array and is unmounted, but updating the running daemon failed — restart hoservad: %w", row.Mountpoint, err)
	}
	logf("%s is out of the array and unmounted: disk %s — safe to physically remove. Its filesystem was not wiped.", row.Mountpoint, describeRemovedDisk(row))
	return nil
}

// postCheck is doc 09 §4 step 6's cache.EvacuationPostCheck over every
// share's own branch on mountpoint, built from the share list rather
// than from the pool's current branches — the disk is no longer one of
// them once it is unpooled.
func (d DiskRemoveDeps) postCheck(ctx context.Context, mountpoint string) error {
	names, err := d.ShareNames(ctx)
	if err != nil {
		return fmt.Errorf("job: disk_remove: listing shares for the post-check: %w", err)
	}
	clean := filepath.Clean(mountpoint)
	shares := make([]cache.Share, 0, len(names))
	for _, name := range names {
		shares = append(shares, cache.Share{Name: name, Branches: []string{filepath.Join(clean, name)}})
	}
	if err := cache.EvacuationPostCheck(clean, shares); err != nil {
		return fmt.Errorf("job: disk_remove: %w", err)
	}
	return nil
}

// contentFilePrefix names SnapRAID's own content files (and their
// temporary copies), which Layout places directly on a data disk's root
// and every snapraid.conf excludes.
const contentFilePrefix = "snapraid.content"

// lostAndFound is the filesystem's own directory, excluded from every
// snapraid.conf, which removeEmptyDirs leaves in place.
const lostAndFound = "lost+found"

// diskLeftover refuses when anything but SnapRAID's own content files at
// the disk's root — and, when allowDirs is set, directories — is left
// anywhere on the disk: files outside every share (which the evacuation
// never moves and the post-check never looks at) would leave the pool
// with the disk. With allowDirs unset, only the root's own lost+found may
// remain.
func diskLeftover(mountpoint string, allowDirs bool) error {
	root := filepath.Clean(mountpoint)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("job: disk_remove: reading %s: %w", path, err)
		}
		if path == root {
			return nil
		}
		atRoot := filepath.Dir(path) == root
		if d.IsDir() {
			if allowDirs {
				return nil
			}
			if atRoot && d.Name() == lostAndFound {
				return filepath.SkipDir
			}
			return fmt.Errorf("job: disk_remove: %s still holds the directory %s", root, path)
		}
		if atRoot && strings.HasPrefix(d.Name(), contentFilePrefix) {
			return nil
		}
		return fmt.Errorf("job: disk_remove: %s still holds %s — move it off the disk first", root, path)
	})
}

// removeEmptyDirs removes every directory under mountpoint, deepest
// first, except mountpoint itself and its lost+found. It only ever calls
// rmdir, which refuses a directory that is not empty and never removes a
// file.
func removeEmptyDirs(mountpoint string) error {
	root := filepath.Clean(mountpoint)
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("job: disk_remove: reading %s: %w", path, err)
		}
		if !d.IsDir() || path == root {
			return nil
		}
		if filepath.Dir(path) == root && d.Name() == lostAndFound {
			return filepath.SkipDir
		}
		dirs = append(dirs, path)
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syscall.Rmdir(dirs[i]); err != nil {
			return fmt.Errorf("job: disk_remove: removing the empty directory %s: %w", dirs[i], err)
		}
	}
	return nil
}

// confirmUntracked refuses unless a fresh diff shows SnapRAID tracking
// no file on mountpoint and seeing none there — including files outside
// every share, which the post-check does not look at. Dropping the data
// line of a disk SnapRAID still tracks files on would take them out of
// parity and out of the pool at once.
func (d DiskRemoveDeps) confirmUntracked(ctx context.Context, mountpoint string) error {
	report, err := d.Parity.Diff(ctx)
	if err != nil {
		return fmt.Errorf("job: disk_remove: reading SnapRAID's view of %s after the sync: %w", mountpoint, err)
	}
	counts, ok := report.PerDisk[mountpoint]
	if !ok {
		return fmt.Errorf("job: disk_remove: SnapRAID's diff does not list %s — refusing to drop it from snapraid.conf", mountpoint)
	}
	if counts.FilesBefore != 0 || counts.FilesAfter != 0 {
		return fmt.Errorf("job: disk_remove: SnapRAID still records %d files on %s and sees %d there now — refusing to drop it from snapraid.conf", counts.FilesBefore, mountpoint, counts.FilesAfter)
	}
	return nil
}

// mountedAs reports whether mountpoint is mounted, and refuses when what
// is mounted there is not the filesystem uuid names.
func mountedAs(ctx context.Context, m mountReader, mountpoint, uuid string) (bool, error) {
	mounted, err := m.IsMounted(ctx, mountpoint)
	if err != nil {
		return false, fmt.Errorf("job: disk_remove: reading whether %s is mounted: %w", mountpoint, err)
	}
	if !mounted {
		return false, nil
	}
	got, err := m.MountedUUID(ctx, mountpoint)
	if err != nil {
		return false, fmt.Errorf("job: disk_remove: reading the filesystem mounted at %s: %w", mountpoint, err)
	}
	if got != uuid {
		return false, fmt.Errorf("job: disk_remove: %s is mounted with filesystem %s, not the array disk's %s — refusing", mountpoint, got, uuid)
	}
	return true, nil
}

func dataDiskRow(disks []store.ArrayDisk, mountpoint string) (store.ArrayDisk, bool) {
	for _, d := range disks {
		if d.Role == store.ArrayRoleData && filepath.Clean(d.Mountpoint) == mountpoint {
			return d, true
		}
	}
	return store.ArrayDisk{}, false
}

// describeRemovedDisk names the disk the way a person finds it in the
// case: its device, and its WWN and serial when known.
func describeRemovedDisk(d store.ArrayDisk) string {
	parts := []string{d.Device}
	if d.WWN != "" {
		parts = append(parts, "WWN "+d.WWN)
	}
	if d.Serial != "" {
		parts = append(parts, "serial "+d.Serial)
	}
	return strings.Join(parts, ", ")
}
