package migrate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mdg-labs/hoserva/internal/disk"
)

var (
	// ErrImportNoReport is returned when the import is asked for before a scan
	// has produced a report.
	ErrImportNoReport = errors.New("there is no finished scan to import from: scan the Unraid flash first")
	// ErrImportScanUnfinished is returned while a scan is running or the latest
	// one failed: the mapping is confirmed against a complete report only.
	ErrImportScanUnfinished = errors.New("the latest scan has not finished cleanly: scan again before importing")
	// ErrImportNoGo is returned when the report's verdict is no-go.
	ErrImportNoGo = errors.New("the scan's verdict is no-go: fix what it refuses and scan again before importing")
	// ErrImportNoReview is returned for a report made before the scan kept the
	// disk table the import checks the mapping against.
	ErrImportNoReview = errors.New("the report has no disk table to check the mapping against: scan again before importing")
	// ErrImportDiskRefused is returned when a role other than ignore is given to
	// a disk the scan refused.
	ErrImportDiskRefused = errors.New("the scan refused this disk")
	// ErrImportUnknownDisk is returned when a role is given to a disk the
	// scan's disk table does not list.
	ErrImportUnknownDisk = errors.New("the scan did not list this disk: scan again with it attached")
	// ErrImportRole is returned when a role contradicts what the capture says
	// the disk is.
	ErrImportRole = errors.New("this role is not allowed for this disk")
	// ErrImportNotConfigured is returned when this daemon cannot list disks.
	ErrImportNotConfigured = errors.New("this daemon cannot list this machine's disks")
)

// importRoleErrors are the refusals of a disk-role mapping: the mapping is
// wrong, not the daemon.
var importRoleErrors = []error{
	ErrImportDiskRefused, ErrImportUnknownDisk, ErrImportRole,
	disk.ErrAdoptDiskMissing, disk.ErrAdoptDiskAmbiguous, disk.ErrAdoptBootDisk, disk.ErrAdoptUnraidBoot,
	disk.ErrAdoptDiskFailed, disk.ErrAdoptNoFilesystem, disk.ErrAdoptUUIDShared, disk.ErrAdoptDiskChanged,
	disk.ErrAdoptNoCacheBinding, disk.ErrNoParityDisks, disk.ErrTooManyParityDisks, disk.ErrNoDataDisks,
	disk.ErrParityTooSmall, disk.ErrWeakIdentityParity, disk.ErrDeviceAssignedTwice, disk.ErrMissingSize,
	disk.ErrUnsupportedFilesystem, disk.ErrBootPartitionNotCache, disk.ErrBootPartitionNotSpare,
	disk.ErrBootPartitionAdopt, disk.ErrBootDevice,
}

// IsImportRoleError reports whether err is a refusal of the disk-role mapping
// itself, which the API answers as 400 invalid_import_roles.
func IsImportRoleError(err error) bool {
	for _, target := range importRoleErrors {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// ImportPlan is a disk-role mapping resolved against the scan's report and a
// fresh inventory: the assignments in the order the disks are adopted, and the
// plan they stand for.
type ImportPlan struct {
	Assignments []disk.AdoptionAssignment
	Plan        disk.AdoptionPlan
}

// PlanImport checks the user's disk-role mapping and resolves it, writing
// nothing and opening no disk. It is refused unless the session holds a
// finished scan whose verdict is not no-go, and every role names a disk the
// scan listed that is still attached. Beyond the rules disk.ResolveAdoption
// applies to the inventory (no Unraid boot device or USB stick in any role but
// ignore, no weak-identity parity, parity at least as large as the largest data
// disk and one or two of it, no disk twice, never the disk this machine boots
// from), it refuses a disk the scan refused in any role but ignore, a disk
// disks.ini records as parity as data (whatever filesystem it reports), and a
// disk it records as data as parity or cache, which would erase the files
// being adopted. The cache role of an Unraid internal boot device that shares
// its disk with the cache records that device's data partition only.
//
// Data disks are adopted in Unraid's disk order, so /mnt/disk1 is the first of
// them. The job calls PlanImport again, with a fresh inventory, immediately
// before it mounts anything.
func (s *Service) PlanImport(ctx context.Context, assignments []disk.AdoptionAssignment) (*ImportPlan, error) {
	if s.Scanner == nil || s.Scanner.Disks == nil {
		return nil, ErrImportNotConfigured
	}
	s.mu.Lock()
	sess, err := s.load(ctx)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := CheckImportable(sess.Report, sess.Scan != nil); err != nil {
		return nil, err
	}
	listed, err := s.Scanner.Disks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing this machine's disks: %w", err)
	}
	return PlanFromReview(sess.Report.Review, listed, assignments)
}

// CheckImportable refuses an import unless report is a finished scan's whose
// verdict is not no-go and which kept the disk table the mapping is checked
// against. scanUnfinished is true while a scan is running or the latest one
// failed, which leaves a report from an earlier scan in the session.
func CheckImportable(report *Report, scanUnfinished bool) error {
	switch {
	case report == nil:
		return ErrImportNoReport
	case scanUnfinished:
		return ErrImportScanUnfinished
	case report.Verdict == VerdictNoGo:
		return ErrImportNoGo
	case report.Review == nil:
		return ErrImportNoReview
	}
	return nil
}

// PlanFromReview is PlanImport's check of the mapping against the scan's disk
// table and an inventory of this machine's disks.
func PlanFromReview(review *Review, listed []disk.Disk, assignments []disk.AdoptionAssignment) (*ImportPlan, error) {
	type placed struct {
		a    disk.AdoptionAssignment
		num  int
		slot string
	}
	var others, parity, data []placed
	for _, a := range assignments {
		p := placed{a: a}
		// A cache on a spare partition of the boot disk is named by the
		// partition, not by a disk of the scan's table.
		cacheByPartition := a.Role == disk.AdoptCache && a.Serial == "" && a.WWN == ""
		if a.Role != disk.AdoptIgnore && !cacheByPartition {
			// The stick is refused as the stick whatever the scan's table calls it.
			for _, d := range listed {
				if disk.IsUnraidStick(d) && (a.WWN != "" && strings.EqualFold(d.WWN, a.WWN) || a.WWN == "" && a.Serial != "" && d.Serial == a.Serial) {
					return nil, fmt.Errorf("%s: %w", d.Device, disk.ErrUnraidStick)
				}
			}
			row, err := reviewRow(review, a)
			if err != nil {
				return nil, err
			}
			if err := checkRole(row, a); err != nil {
				return nil, err
			}
			if a.Role == disk.AdoptCache && row.UnraidBoot && review.Boot.SharedWithCache != nil && !*review.Boot.SharedWithCache {
				return nil, fmt.Errorf("%w: %s is an Unraid boot device with no cache on it", ErrImportRole, serialOrWWN(a))
			}
			p.num, p.slot = row.DiskNumber, row.Slot
		}
		switch a.Role {
		case disk.AdoptData:
			data = append(data, p)
		case disk.AdoptParity:
			parity = append(parity, p)
		default:
			others = append(others, p)
		}
	}
	sort.SliceStable(data, func(i, j int) bool { return data[i].num < data[j].num })
	sort.SliceStable(parity, func(i, j int) bool { return parity[i].slot < parity[j].slot })
	sorted := make([]disk.AdoptionAssignment, 0, len(assignments))
	for _, group := range [][]placed{others, parity, data} {
		for _, p := range group {
			sorted = append(sorted, p.a)
		}
	}
	plan, err := disk.ResolveAdoption(listed, sorted)
	if err != nil {
		return nil, err
	}
	return &ImportPlan{Assignments: sorted, Plan: plan}, nil
}

// reviewRow is the scan's disk table row for the disk a names, by WWN or else
// serial. A disk the capture names but this machine did not match has neither
// and is not found.
func reviewRow(review *Review, a disk.AdoptionAssignment) (ReviewDisk, error) {
	var found []ReviewDisk
	for _, d := range review.Disks {
		switch {
		case a.WWN != "" && strings.EqualFold(d.WWN, a.WWN):
			found = append(found, d)
		case a.WWN == "" && a.Serial != "" && d.Serial == a.Serial:
			found = append(found, d)
		}
	}
	switch len(found) {
	case 0:
		return ReviewDisk{}, fmt.Errorf("%w: %s", ErrImportUnknownDisk, serialOrWWN(a))
	case 1:
		return found[0], nil
	}
	return ReviewDisk{}, fmt.Errorf("%w: %s", disk.ErrAdoptDiskAmbiguous, serialOrWWN(a))
}

func serialOrWWN(a disk.AdoptionAssignment) string {
	if a.WWN != "" {
		return "WWN " + a.WWN
	}
	return "serial " + a.Serial
}

// checkRole applies the rules that come from what the scan and the capture say
// about the disk.
func checkRole(row ReviewDisk, a disk.AdoptionAssignment) error {
	name := serialOrWWN(a)
	switch {
	case row.Refused:
		return fmt.Errorf("%w: %s (%s): %s", ErrImportDiskRefused, name, row.RefusalCode, row.Refusal)
	case row.UnraidRole == UnraidBoot:
		return fmt.Errorf("%w: %s is an Unraid boot device, which can only be ignored", ErrImportRole, name)
	case row.UnraidRole == UnraidParity && a.Role == disk.AdoptData:
		return fmt.Errorf("%w: %s is Unraid's parity disk, and its filesystem signature is not data: parity is never mounted", ErrImportRole, name)
	case row.UnraidRole == UnraidData && (a.Role == disk.AdoptParity || a.Role == disk.AdoptCache):
		return fmt.Errorf("%w: %s is one of Unraid's data disks, and the point of no return would erase the files being adopted", ErrImportRole, name)
	case row.HostBoot != nil && *row.HostBoot && a.Role != disk.AdoptIgnore:
		return fmt.Errorf("%w: %s is the disk this machine boots from; only a spare partition of it can be the cache", disk.ErrAdoptBootDisk, name)
	}
	return nil
}
