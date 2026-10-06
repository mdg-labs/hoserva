package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// The point of no return (doc 05 §4 step 17): the first step of the migration
// that writes to a disk of the old array. This file is the service's part of
// it: the gate that offers it only after a passing verify, the plan of what it
// would erase and the typed confirmation that names it. The formatting is
// disk.FormatParityInit, run by the migration_parity job (internal/job).

var (
	// ErrVerifyRequired is returned when the parity initialisation is asked for
	// and the latest verify of the adopted array did not pass, or none has run
	// since the import last ran.
	ErrVerifyRequired = errors.New("the adopted array has no passing verify: verify it and check that it passes before parity is touched")
	// ErrParityNotPending is returned when the parity initialisation is asked for
	// and no import is waiting for its point of no return.
	ErrParityNotPending = errors.New("there is no adopted Unraid array waiting for its point of no return: import the data disks first")
	// ErrParityNotConfigured is returned when this daemon cannot initialise parity.
	ErrParityNotConfigured = errors.New("this daemon cannot initialise parity")
	// ErrMirroredBootPool is returned when the cache is a partition of an Unraid
	// internal boot device and the capture does not say the boot pool is not a
	// mirrored pair.
	ErrMirroredBootPool = errors.New("the cache is a partition of an Unraid boot device that may be one of a mirrored pair, which is never formatted: choose another cache or none")
	// ErrPoolBelowMinFreeSpace is returned when no adopted data disk has the
	// catch-all pool's minfreespace free: every directory the step makes through
	// /mnt/user would fail with ENOSPC after the former parity disk and the cache
	// were erased.
	ErrPoolBelowMinFreeSpace = errors.New("the pool could not create the share directories the migration needs once the point of no return is crossed")
)

// ParityInitWindow is the unprotected window in doc 05 §5's terms, stated at the
// point where the user is about to cross the line.
const ParityInitWindow = "Between the moment Unraid's array stopped and the moment the initial sync completes, the array has no redundancy whatsoever. " +
	"Unraid's parity is destroyed when the former parity disk(s) are formatted now, and SnapRAID's parity does not exist until the initial sync finishes, which takes hours and is I/O-heavy. " +
	"A disk that fails in that window loses its data. Irreplaceable data must have a backup that is not this array; do not write significant new data until the sync has completed."

// ParityErase is one device the point of no return erases.
type ParityErase struct {
	// Role is "parity" or "cache".
	Role   string
	Device string
	Serial string
	WWN    string
	Size   int64
	// Partition is true when only a partition of Device's disk is erased: the
	// cache of an Unraid boot + data device or a spare partition of the boot
	// disk. The rest of that disk is left alone.
	Partition bool
}

// ParityInit says what the point of no return does now, for the screen and the
// command that ask the user to confirm it. It is nil when the migration is not
// at a point where it is offered.
type ParityInit struct {
	// Finishing is true when the former parity and cache disks are already
	// formatted and recorded and only the rest of the initialisation is left:
	// nothing is erased then, and Confirmation is disk.ParityInitFinishConfirmation.
	Finishing bool
	// Confirmation is the exact string that must be typed back, naming every
	// device erased. Empty when Problem is set.
	Confirmation string
	Erases       []ParityErase
	// Problem says why it cannot be offered now (a disk missing or changed since
	// the import).
	Problem string
	// Window is ParityInitWindow. Rollback says what going back means for this
	// session's boot mode and layout (doc 05 §5).
	Window   string
	Rollback []string
}

// InvalidateVerify forgets the verify result: an import that runs again changes
// what the pool shows, so a verify made before it says nothing about what is
// mounted now. The import job calls it before it changes anything.
func (s *Service) InvalidateVerify(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.load(ctx)
	if err != nil {
		return err
	}
	if sess.Verify == nil {
		return nil
	}
	sess.Verify = nil
	return s.save(ctx, sess)
}

// PlanParityInit is the gate of the point of no return and its plan: it refuses
// unless an import is pending and the latest verify of the adopted array
// passed (ErrVerifyRequired; a run clears the earlier result, and an import that
// runs again clears it too, so a pass is only ever the latest run's on what is
// mounted now), and resolves the recorded data, parity and cache disks again
// from a fresh inventory by identity, with every rule the import applied. A disk
// that is gone, swapped or ambiguous refuses it. The plan's erase targets are the
// former parity disks and the cache; the data disks are never in them.
//
// A cache that is a partition of an Unraid internal boot device is refused
// unless the capture says the boot pool is not a mirrored pair (ErrMirroredBootPool),
// and disk.FormatParityInit refuses it again whenever more than one Unraid boot
// device is attached: a partition of a mirrored boot pool is never formatted, and
// neither is the other member's.
func (s *Service) PlanParityInit(ctx context.Context) (disk.AdoptionPlan, error) {
	if s.Pending == nil {
		return disk.AdoptionPlan{}, ErrParityNotPending
	}
	pending, err := s.Pending(ctx)
	if err != nil {
		return disk.AdoptionPlan{}, fmt.Errorf("reading whether an import is pending: %w", err)
	}
	if !pending {
		return disk.AdoptionPlan{}, ErrParityNotPending
	}
	if s.Record == nil || s.Scanner == nil || s.Scanner.Disks == nil {
		return disk.AdoptionPlan{}, ErrParityNotConfigured
	}
	s.mu.Lock()
	sess, err := s.load(ctx)
	s.mu.Unlock()
	if err != nil {
		return disk.AdoptionPlan{}, err
	}
	switch {
	case sess.Verify == nil:
		return disk.AdoptionPlan{}, fmt.Errorf("%w: no verify has run since the import", ErrVerifyRequired)
	case sess.Verify.Status == VerifyRunning:
		return disk.AdoptionPlan{}, fmt.Errorf("%w: a verify is still running", ErrVerifyRequired)
	case sess.Verify.Status != VerifyPassed:
		return disk.AdoptionPlan{}, fmt.Errorf("%w: the latest verify failed", ErrVerifyRequired)
	}
	if err := CheckImportable(sess.Report, sess.Scan != nil); err != nil {
		return disk.AdoptionPlan{}, err
	}
	data, recorded, err := s.Record(ctx)
	if err != nil {
		return disk.AdoptionPlan{}, fmt.Errorf("reading the adoption's record: %w", err)
	}
	listed, err := s.Scanner.Disks.List(ctx)
	if err != nil {
		return disk.AdoptionPlan{}, fmt.Errorf("listing this machine's disks: %w", err)
	}
	plan, err := PlanFromReview(sess.Report.Review, listed, recordedAssignments(listed, data, recorded))
	if err != nil {
		return disk.AdoptionPlan{}, err
	}
	if err := CheckBootCacheNotMirrored(sess.Report.Review, listed, plan.Plan); err != nil {
		return disk.AdoptionPlan{}, err
	}
	if err := s.checkPoolCreatable(ctx, data); err != nil {
		return disk.AdoptionPlan{}, err
	}
	return plan.Plan, nil
}

// checkPoolCreatable refuses (ErrPoolBelowMinFreeSpace) unless some adopted data
// disk has the catch-all's minfreespace free (doc 02 §1): once the disks are
// mounted read-write, mergerfs places a new directory only on a branch with that
// much room, and answers ENOSPC for the share mount points the point of no
// return makes when none has. It is one statfs(2) of each data disk, made at the
// user's request, and a disk that cannot be read refuses it as well.
func (s *Service) checkPoolCreatable(ctx context.Context, data []store.ArrayDisk) error {
	var mounts []string
	for _, d := range data {
		if d.Role == store.ArrayRoleData {
			mounts = append(mounts, d.Mountpoint)
		}
	}
	statter := s.Space
	if statter == nil {
		statter = pool.StatfsSpaceStatter{}
	}
	floor := s.MinFreeSpace
	if floor == "" {
		floor = pool.DefaultOptions().MinFreeSpace
	}
	if err := pool.CheckCreatable(ctx, statter, mounts, floor); err != nil {
		var below *pool.BelowMinFreeSpaceError
		if errors.As(err, &below) {
			return fmt.Errorf("%w: no data disk has the pool's minimum free space of %s, and %s has the most, %s: make room on a data disk (in Unraid, or by undoing the import) and import again",
				ErrPoolBelowMinFreeSpace, below.MinFreeSpace, below.LargestPath, formatBytes(below.LargestFreeBytes))
		}
		return fmt.Errorf("checking the data disks' free space: %w", err)
	}
	return nil
}

// CheckBootCacheNotMirrored refuses (ErrMirroredBootPool) a cache that is a
// partition of an Unraid internal boot device unless the capture says the boot
// pool is not a mirrored pair: formatting a partition of a mirrored pair's
// member is never done, and a capture that does not say is not read as "not
// mirrored". A partition whose disk cannot be identified (no listed disk, or
// more than one, has its identity) is refused the same way, as the check cannot
// run. Any other cache passes.
func CheckBootCacheNotMirrored(review *Review, listed []disk.Disk, plan disk.AdoptionPlan) error {
	c := plan.Cache
	if c == nil || !disk.IsPartition(c.Device, c.ByIDName) {
		return nil
	}
	parent, err := parentOf(listed, c.WWN, c.Serial)
	if err != nil {
		return fmt.Errorf("%w (%s): its disk cannot be identified: %v", ErrMirroredBootPool, c.Device, err)
	}
	if !parent.UnraidBoot {
		return nil
	}
	if review == nil || review.Boot.Mirrored == nil || *review.Boot.Mirrored {
		return fmt.Errorf("%w (%s)", ErrMirroredBootPool, c.Device)
	}
	return nil
}

func parentOf(listed []disk.Disk, wwn, serial string) (disk.Disk, error) {
	var found []disk.Disk
	for _, d := range listed {
		if wwn != "" && strings.EqualFold(d.WWN, wwn) || wwn == "" && serial != "" && d.Serial == serial {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		return disk.Disk{}, fmt.Errorf("%d disks have the identity %s", len(found), serialOrWWN(disk.AdoptionAssignment{WWN: wwn, Serial: serial}))
	}
	return found[0], nil
}

// recordedAssignments is the disk-role mapping the import confirmed, rebuilt
// from what it recorded: its data disks and its former parity and cache disks,
// each by the identity recorded, never the device name. A cache recorded as a
// partition is the spare partition of the boot disk when its disk is the one
// this machine boots from (named by its by-id name and PARTUUID, as at import),
// and otherwise the data partition of an Unraid boot device (named by its
// disk's identity, from which the plan finds the partition again).
func recordedAssignments(listed []disk.Disk, data []store.ArrayDisk, recorded []store.RecordedDisk) []disk.AdoptionAssignment {
	var out []disk.AdoptionAssignment
	for _, r := range recorded {
		switch r.Role {
		case store.ArrayRoleParity:
			out = append(out, disk.AdoptionAssignment{Role: disk.AdoptParity, Serial: r.Serial, WWN: r.WWN})
		case store.ArrayRoleCache:
			if r.PartUUID != "" {
				if parent, err := parentOf(listed, r.WWN, r.Serial); err == nil && parent.Boot {
					out = append(out, disk.AdoptionAssignment{Role: disk.AdoptCache, ByIDName: r.ByIDName, PartUUID: r.PartUUID})
					continue
				}
			}
			out = append(out, disk.AdoptionAssignment{Role: disk.AdoptCache, Serial: r.Serial, WWN: r.WWN})
		}
	}
	for _, d := range data {
		if d.Role == store.ArrayRoleData {
			out = append(out, disk.AdoptionAssignment{Role: disk.AdoptData, Serial: d.Serial, WWN: d.WWN})
		}
	}
	return out
}

// ParityInit is what the point of no return would do now, for the migration
// screen and command: the confirmation to type, every device that will be
// erased, the unprotected window and what rollback means for this session's
// layout. It is nil unless the phase is verified (a passing verify of an import
// that is pending) or initializing (a run that stopped after the disks were
// formatted). A plan that cannot be made says why in Problem and offers no
// confirmation.
func (s *Service) ParityInit(ctx context.Context) (*ParityInit, error) {
	st, err := s.State(ctx)
	if err != nil {
		return nil, err
	}
	var rollback []string
	if st.Report != nil {
		rollback = RollbackNotes(st.Report.Review)
	}
	info := &ParityInit{Window: ParityInitWindow, Rollback: rollback}
	switch st.Phase {
	case PhaseInitializing:
		info.Finishing = true
		info.Confirmation = disk.ParityInitFinishConfirmation
		return info, nil
	case PhaseVerified:
	default:
		return nil, nil
	}
	plan, err := s.PlanParityInit(ctx)
	if err != nil {
		info.Problem = err.Error()
		return info, nil
	}
	return ParityInitOf(plan, st.Report.Review), nil
}

// ParityInitOf is what the point of no return would do to plan: the
// confirmation naming every device erased, those devices, the unprotected
// window and what rollback means for the review's layout.
func ParityInitOf(plan disk.AdoptionPlan, review *Review) *ParityInit {
	info := &ParityInit{Window: ParityInitWindow, Rollback: RollbackNotes(review), Confirmation: plan.ParityInitConfirmation()}
	for _, p := range plan.Parity {
		info.Erases = append(info.Erases, ParityErase{Role: "parity", Device: p.Device, Serial: p.Serial, WWN: p.WWN, Size: p.Size})
	}
	if c := plan.Cache; c != nil {
		info.Erases = append(info.Erases, ParityErase{Role: "cache", Device: c.Device, Serial: c.Serial, WWN: c.WWN, Size: c.Size, Partition: disk.IsPartition(c.Device, c.ByIDName)})
	}
	return info
}

// ExpectedParityConfirmation is the string the typed confirmation must equal
// now. The request is refused with disk.ErrConfirmationMismatch unless it is, and
// again by the job against a fresh inventory.
func (s *Service) ExpectedParityConfirmation(ctx context.Context) (string, error) {
	finishing, err := s.finishing(ctx)
	if err != nil {
		return "", err
	}
	if finishing {
		return disk.ParityInitFinishConfirmation, nil
	}
	plan, err := s.PlanParityInit(ctx)
	if err != nil {
		return "", err
	}
	return plan.ParityInitConfirmation(), nil
}

func (s *Service) finishing(ctx context.Context) (bool, error) {
	if s.Finishing == nil {
		return false, nil
	}
	finishing, err := s.Finishing(ctx)
	if err != nil {
		return false, fmt.Errorf("reading whether a parity initialisation is unfinished: %w", err)
	}
	return finishing, nil
}

// RollbackNotes says what going back means once the point of no return is
// crossed, for the layout the scan's review recorded (doc 05 §5's table). What
// the review does not say is not guessed: a boot mode it does not know is a
// general statement only.
func RollbackNotes(rv *Review) []string {
	notes := []string{
		"Until you confirm, going back is clean: nothing has been written to the data disks. Once the former parity disk(s) are formatted, Unraid's parity is destroyed and its array cannot be started as it was: going back means restoring from backup. The cache device or partition is erased too.",
	}
	if rv == nil {
		return notes
	}
	switch rv.Boot.Mode {
	case "usb":
		sharedCache, unknown := false, false
		for _, d := range rv.Disks {
			if d.UnraidRole != UnraidCache {
				continue
			}
			switch {
			case d.HostBoot == nil:
				unknown = true
			case *d.HostBoot:
				sharedCache = true
			}
		}
		if sharedCache || unknown {
			notes = append(notes, "Unraid boots from a USB stick and Debian shares the cache's NVMe: to boot Unraid again, reinsert the stick, re-create the Unraid cache and move appdata back.")
		}
		if !sharedCache {
			notes = append(notes, "Unraid boots from a USB stick and Debian is on a separate device: to boot Unraid again, reinsert the stick and boot.")
		}
	case "internal":
		mirrored, shared := rv.Boot.Mirrored, rv.Boot.SharedWithCache
		switch {
		case mirrored != nil && *mirrored:
			notes = append(notes,
				"Unraid boots from an internal mirrored pair: if Debian is on another device, switch the firmware boot order back; if Debian is on one device of the pair, switch it to the other device (Unraid runs with a degraded boot pool) and replace the wiped device through its normal drive assignment; if Debian is on both, restore the Flash Backup zip to a USB stick and boot it.")
		case mirrored != nil && shared != nil && *shared:
			notes = append(notes, "Unraid boots from an internal device that also holds its cache: if Debian is on another device, switch the firmware boot order back (partition 4, the cache, is erased now); if Debian is on the same NVMe, restore the Flash Backup zip to a USB stick, boot it and re-create the cache.")
		case mirrored != nil && shared != nil:
			notes = append(notes, "Unraid boots from a dedicated internal device: if Debian is on another device, switch the firmware boot order back; if Debian is on that device, restore the Flash Backup zip to a USB stick and boot it.")
		}
	}
	return notes
}
