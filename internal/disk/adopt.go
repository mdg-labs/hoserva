package disk

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// AdoptionRole is the role a disk is given in an adoption (doc 05 §4 Phase C).
type AdoptionRole string

const (
	AdoptParity AdoptionRole = "parity"
	AdoptData   AdoptionRole = "data"
	AdoptCache  AdoptionRole = "cache"
	AdoptIgnore AdoptionRole = "ignore"
)

var (
	// ErrAdoptDiskMissing refuses an assignment that no disk of this machine
	// matches by WWN or serial.
	ErrAdoptDiskMissing = errors.New("disk: no disk on this machine has the identity given")
	// ErrAdoptDiskAmbiguous refuses an assignment that more than one disk of this
	// machine matches: a role is never given to the first of several candidates
	// (Q21).
	ErrAdoptDiskAmbiguous = errors.New("disk: more than one disk on this machine has the identity given")
	// ErrAdoptBootDisk refuses the disk this machine boots from in any role but
	// the cache, and as the cache a whole one: only a spare partition of it can
	// be the cache (doc 01 §6).
	ErrAdoptBootDisk = errors.New("disk: this is the disk this machine boots from")
	// ErrAdoptUnraidBoot refuses an Unraid boot device (an internal boot pool)
	// in any role but ignore, and as the cache unless it has the data partition
	// Unraid keeps its cache on.
	ErrAdoptUnraidBoot = errors.New("disk: this is an Unraid boot device, which is only ever left alone")
	// ErrAdoptDiskFailed refuses a disk reported failed.
	ErrAdoptDiskFailed = errors.New("disk: this disk is reported failed")
	// ErrAdoptNoFilesystem refuses a data disk whose filesystem cannot be
	// mounted: it has none Hoserva adopts, no device node or no UUID.
	ErrAdoptNoFilesystem = errors.New("disk: this data disk has no filesystem Hoserva can adopt in place")
	// ErrAdoptUUIDShared refuses a data disk whose filesystem UUID another disk
	// also carries when nothing but that UUID could tell the two apart: the
	// other is a data disk too, or this one has no by-id link to bind the mount
	// to.
	ErrAdoptUUIDShared = errors.New("disk: this data disk's filesystem UUID is also on another disk")
	// ErrAdoptDiskChanged refuses a disk that is not what the confirmed plan
	// named: its identity, filesystem, UUID or size is different now.
	ErrAdoptDiskChanged = errors.New("disk: a disk is not the one the confirmed mapping named")
	// ErrAdoptNoCacheBinding refuses a cache assignment that names neither a
	// disk by serial or WWN nor a spare boot-disk partition by its by-id name
	// and PARTUUID.
	ErrAdoptNoCacheBinding = errors.New("disk: the cache is named by a disk's serial or WWN, or a spare boot-disk partition's by-id name and PARTUUID")
	// ErrTooManyCacheDisks refuses a second cache assignment: the first one
	// the user confirmed is never replaced.
	ErrTooManyCacheDisks = errors.New("disk: at most one cache disk can be assigned")
)

// AdoptionAssignment is one disk of an adoption request: the role the user
// gave it, keyed by the stable identity the user confirmed against the serial
// table (Q21), never by a /dev name. A cache on a spare partition of the boot
// disk (doc 01 §6) is keyed by that partition's by-id name and PARTUUID
// instead, with no serial or WWN.
type AdoptionAssignment struct {
	Role     AdoptionRole `json:"role"`
	Serial   string       `json:"serial,omitempty"`
	WWN      string       `json:"wwn,omitempty"`
	ByIDName string       `json:"byId,omitempty"`
	PartUUID string       `json:"partUuid,omitempty"`
}

// AdoptedDisk is a data disk adopted in place: it is mounted read-only with the
// filesystem it has and never formatted. The embedded AssignedDisk has Adopt
// set and the disk's own filesystem UUID; FSDevice is the node the filesystem
// is on when the plan was resolved (transient, never compared), and
// MountSource is the /dev/disk/by-id path of that filesystem's device, or
// empty when the mount is by UUID (the UUID is then the disk's alone).
type AdoptedDisk struct {
	AssignedDisk
	Size        int64  `json:"size"`
	FSDevice    string `json:"fsDevice"`
	MountSource string `json:"mountSource,omitempty"`
}

// RecordedDisk is a parity or cache disk of an adoption: its identity is
// recorded and nothing else is done to it until the point of no return
// (doc 05 §4 step 17). It is never formatted, mounted or opened.
type RecordedDisk struct {
	AssignedDisk
	Size int64 `json:"size"`
}

// AdoptionPlan is TopologyPlan's counterpart for the Unraid adoption: the data
// disks adopted as they are, and the former parity and cache disks recorded and
// left untouched. TopologyPlan cannot express it: it formats its parity disks
// at once, and mounts every disk read-write.
type AdoptionPlan struct {
	Data   []AdoptedDisk  `json:"data"`
	Parity []RecordedDisk `json:"parity"`
	Cache  *RecordedDisk  `json:"cache,omitempty"`
}

func sameDisk(d Disk, wwn, serial string) bool {
	if wwn != "" {
		return strings.EqualFold(d.WWN, wwn)
	}
	return serial != "" && d.Serial == serial
}

// findAdoptable returns the one disk of listed that wwn or serial names.
func findAdoptable(listed []Disk, wwn, serial string) (Disk, error) {
	if wwn == "" && serial == "" {
		return Disk{}, fmt.Errorf("%w: neither a serial nor a WWN was given", ErrAdoptDiskMissing)
	}
	var found []Disk
	for _, d := range listed {
		if sameDisk(d, wwn, serial) {
			found = append(found, d)
		}
	}
	switch len(found) {
	case 0:
		return Disk{}, fmt.Errorf("%w: %s", ErrAdoptDiskMissing, orSerial(wwn, serial))
	case 1:
		if wwn != "" && serial != "" && found[0].Serial != "" && found[0].Serial != serial {
			return Disk{}, fmt.Errorf("%w: WWN %s is on a disk whose serial is %s, not %s", ErrAdoptDiskChanged, wwn, found[0].Serial, serial)
		}
		return found[0], nil
	}
	return Disk{}, fmt.Errorf("%w: %s", ErrAdoptDiskAmbiguous, orSerial(wwn, serial))
}

func orSerial(wwn, serial string) string {
	if wwn != "" {
		return "WWN " + wwn
	}
	return "serial " + serial
}

// ResolveAdoption turns the user's assignments into a plan, from a fresh
// inventory. It refuses, naming the disk: an assignment no disk matches or
// several do, a disk given two roles, the disk this machine boots from, an
// Unraid boot device in any role but ignore (as the cache it records its data
// partition alone, UnraidCachePartition) or the Unraid USB stick in any role
// but ignore, a failed
// disk, a data disk with no filesystem Hoserva adopts in place, a data disk
// whose filesystem UUID cannot be told from another disk's, and every rule of
// Validate. A disk with the ignore role is not looked up: nothing is done to
// it. It writes nothing and opens no device.
func ResolveAdoption(listed []Disk, assignments []AdoptionAssignment) (AdoptionPlan, error) {
	var plan AdoptionPlan
	taken := map[string]bool{}
	take := func(dev string) error {
		if taken[dev] {
			return fmt.Errorf("%w: %s", ErrDeviceAssignedTwice, dev)
		}
		taken[dev] = true
		return nil
	}
	for _, a := range assignments {
		switch a.Role {
		case AdoptIgnore:
			d, err := findAdoptable(listed, a.WWN, a.Serial)
			if err != nil {
				return AdoptionPlan{}, err
			}
			if err := take(d.Device); err != nil {
				return AdoptionPlan{}, err
			}
			continue
		case AdoptCache:
			if a.ByIDName != "" || a.PartUUID != "" {
				rec, err := resolveCachePartition(listed, a)
				if err != nil {
					return AdoptionPlan{}, err
				}
				if plan.Cache != nil {
					return AdoptionPlan{}, ErrTooManyCacheDisks
				}
				if err := take(rec.Device); err != nil {
					return AdoptionPlan{}, err
				}
				plan.Cache = &rec
				continue
			}
		case AdoptParity, AdoptData:
		default:
			return AdoptionPlan{}, fmt.Errorf("disk: unknown role %q", a.Role)
		}
		d, err := findAdoptable(listed, a.WWN, a.Serial)
		if err != nil {
			return AdoptionPlan{}, err
		}
		if err := take(d.Device); err != nil {
			return AdoptionPlan{}, err
		}
		if err := refuseAdoptable(d, a.Role); err != nil {
			return AdoptionPlan{}, err
		}
		if a.Role == AdoptCache && d.UnraidBoot {
			rec := unraidCachePartition(d)
			if plan.Cache != nil {
				return AdoptionPlan{}, ErrTooManyCacheDisks
			}
			plan.Cache = &rec
			continue
		}
		assigned := AssignedDisk{
			Device: d.Device, WWN: d.WWN, Serial: d.Serial, WeakIdentity: d.WeakIdentity,
			ByIDName: d.ByIDName, FSUUID: d.FSUUID,
		}
		switch a.Role {
		case AdoptData:
			ad, err := adoptData(listed, d, assigned)
			if err != nil {
				return AdoptionPlan{}, err
			}
			plan.Data = append(plan.Data, ad)
		case AdoptParity:
			assigned.Filesystem = XFS
			plan.Parity = append(plan.Parity, RecordedDisk{AssignedDisk: assigned, Size: d.Size})
		case AdoptCache:
			if plan.Cache != nil {
				return AdoptionPlan{}, ErrTooManyCacheDisks
			}
			assigned.Filesystem = XFS
			plan.Cache = &RecordedDisk{AssignedDisk: assigned, Size: d.Size}
		}
	}
	if err := plan.Validate(); err != nil {
		return AdoptionPlan{}, err
	}
	return plan, nil
}

// refuseAdoptable is every refusal of a disk that does not depend on its
// role's own rules.
func refuseAdoptable(d Disk, role AdoptionRole) error {
	switch {
	case IsUnraidStick(d):
		return fmt.Errorf("%s: %w", d.Device, ErrUnraidStick)
	case d.UnraidBoot && (role != AdoptCache || !hasUnraidCachePartition(d)):
		return fmt.Errorf("%s: %w", d.Device, ErrAdoptUnraidBoot)
	case d.Boot:
		return fmt.Errorf("%s: %w", d.Device, ErrAdoptBootDisk)
	case d.Failed:
		return fmt.Errorf("%s: %w", d.Device, ErrAdoptDiskFailed)
	}
	return nil
}

func hasUnraidCachePartition(d Disk) bool {
	p := d.UnraidDataPartition
	return p != nil && p.ByIDName != "" && p.PartUUID != "" && p.Size > 0
}

// unraidCachePartition records the cache of an Unraid internal boot device that
// shares its disk with the cache: partition 4 only, by its own by-id link and
// PARTUUID, never the disk. Partitions 1 to 3 are Unraid's boot pool and are
// never recorded.
func unraidCachePartition(d Disk) RecordedDisk {
	p := d.UnraidDataPartition
	return RecordedDisk{
		AssignedDisk: AssignedDisk{
			Device: p.Device, WWN: d.WWN, Serial: d.Serial, WeakIdentity: d.WeakIdentity,
			ByIDName: p.ByIDName, PartUUID: p.PartUUID, Filesystem: XFS,
		},
		Size: p.Size,
	}
}

func adoptData(listed []Disk, d Disk, assigned AssignedDisk) (AdoptedDisk, error) {
	fs := FilesystemType(d.Filesystem)
	if !adoptableFilesystems[fs] || !strings.HasPrefix(d.FSDevice, "/dev/") || d.FSUUID == "" {
		return AdoptedDisk{}, fmt.Errorf("%s: %w (it holds %q, node %q, UUID %q)", d.Device, ErrAdoptNoFilesystem, d.Filesystem, d.FSDevice, d.FSUUID)
	}
	assigned.Filesystem, assigned.Adopt = fs, true
	ad := AdoptedDisk{AssignedDisk: assigned, Size: d.Size, FSDevice: d.FSDevice}
	var shared []string
	for _, other := range listed {
		if other.Device != d.Device && other.FSUUID != "" && strings.EqualFold(other.FSUUID, d.FSUUID) {
			shared = append(shared, other.Device)
		}
	}
	if d.FSByIDName != "" {
		ad.MountSource = byIDDir + "/" + d.FSByIDName
		return ad, nil
	}
	if len(shared) > 0 {
		sort.Strings(shared)
		return AdoptedDisk{}, fmt.Errorf("%s: %w (also on %s, and this disk has no by-id link to bind the mount to)", d.Device, ErrAdoptUUIDShared, strings.Join(shared, ", "))
	}
	return ad, nil
}

func resolveCachePartition(listed []Disk, a AdoptionAssignment) (RecordedDisk, error) {
	if a.ByIDName == "" || a.PartUUID == "" || a.Serial != "" || a.WWN != "" {
		return RecordedDisk{}, ErrAdoptNoCacheBinding
	}
	for _, d := range listed {
		if !d.Boot {
			continue
		}
		for _, c := range d.CachePartitions {
			if c.ByIDName != a.ByIDName || !strings.EqualFold(c.PartUUID, a.PartUUID) {
				continue
			}
			bound, size, _, err := BindBootPartition(listed, AssignedDisk{Device: c.Device}, true)
			if err != nil {
				return RecordedDisk{}, err
			}
			bound.Filesystem = XFS
			return RecordedDisk{AssignedDisk: bound, Size: size}, nil
		}
	}
	return RecordedDisk{}, fmt.Errorf("%w: no spare partition of the boot disk has by-id name %s and PARTUUID %s", ErrBootPartitionNotSpare, a.ByIDName, a.PartUUID)
}

// Validate checks the array rules of doc 02 §2 and §5 that do not need an
// inventory: one or two parity disks (Q19), each at least as large as the
// largest data disk (Q20) and not weak-identity (Q21), at least one data disk,
// every data disk a filesystem Q23 adopts and distinct from every other data
// disk's UUID, and no device in two roles. Unlike TopologyPlan.Validate, a
// parity disk is recorded, not formatted.
func (p AdoptionPlan) Validate() error {
	switch {
	case len(p.Parity) == 0:
		return ErrNoParityDisks
	case len(p.Parity) > 2:
		return ErrTooManyParityDisks
	case len(p.Data) == 0:
		return ErrNoDataDisks
	}
	seen := map[string]bool{}
	once := func(dev string) error {
		if seen[dev] {
			return fmt.Errorf("%w: %s", ErrDeviceAssignedTwice, dev)
		}
		seen[dev] = true
		return nil
	}
	var maxData int64
	uuids := map[string]string{}
	for _, d := range p.Data {
		if err := once(d.Device); err != nil {
			return err
		}
		if !d.Adopt || !adoptableFilesystems[d.Filesystem] {
			return fmt.Errorf("%w: %s on %s", ErrUnsupportedFilesystem, d.Filesystem, d.Device)
		}
		if d.Size <= 0 {
			return fmt.Errorf("%w: %s", ErrMissingSize, d.Device)
		}
		if other, dup := uuids[strings.ToLower(d.FSUUID)]; dup {
			return fmt.Errorf("%w: %s and %s share %s", ErrAdoptUUIDShared, other, d.Device, d.FSUUID)
		}
		uuids[strings.ToLower(d.FSUUID)] = d.Device
		maxData = max(maxData, d.Size)
	}
	for _, d := range p.Parity {
		if err := once(d.Device); err != nil {
			return err
		}
		if d.WeakIdentity {
			return fmt.Errorf("%w: %s", ErrWeakIdentityParity, d.Device)
		}
		if d.Size <= 0 {
			return fmt.Errorf("%w: %s", ErrMissingSize, d.Device)
		}
		if d.Size < maxData {
			return fmt.Errorf("%w: %s", ErrParityTooSmall, d.Device)
		}
	}
	if p.Cache != nil {
		if err := once(p.Cache.Device); err != nil {
			return err
		}
		if p.Cache.Size <= 0 {
			return fmt.Errorf("%w: %s", ErrMissingSize, p.Cache.Device)
		}
	}
	return nil
}

// MountSourceOrNode is what a data disk's filesystem is read through: its
// identity-bound by-id path when it has one, else its device node.
func (d AdoptedDisk) MountSourceOrNode() string {
	if d.MountSource != "" {
		return d.MountSource
	}
	return d.FSDevice
}

// Matches refuses (ErrAdoptDiskChanged) unless fresh, a plan resolved from a
// new inventory for the same assignments, names the same disks as p did, each
// with the same identity, filesystem, filesystem UUID and size: a disk
// swapped, repartitioned or replaced since the user confirmed the mapping is
// not adopted. Device nodes are not compared; they renumber.
func (p AdoptionPlan) Matches(fresh AdoptionPlan) error {
	changed := func(what, dev string) error {
		return fmt.Errorf("%w: %s (%s)", ErrAdoptDiskChanged, dev, what)
	}
	if len(p.Data) != len(fresh.Data) || len(p.Parity) != len(fresh.Parity) || (p.Cache == nil) != (fresh.Cache == nil) {
		return fmt.Errorf("%w: the set of disks differs", ErrAdoptDiskChanged)
	}
	sameAssigned := func(a, b AssignedDisk) bool {
		return a.WWN == b.WWN && a.Serial == b.Serial && a.ByIDName == b.ByIDName && a.PartUUID == b.PartUUID &&
			a.Filesystem == b.Filesystem && strings.EqualFold(a.FSUUID, b.FSUUID)
	}
	for i, d := range p.Data {
		f := fresh.Data[i]
		if !sameAssigned(d.AssignedDisk, f.AssignedDisk) || d.Size != f.Size || d.MountSource != f.MountSource {
			return changed("identity, filesystem, UUID, size or by-id link", d.Device)
		}
	}
	for i, d := range p.Parity {
		f := fresh.Parity[i]
		if !sameAssigned(d.AssignedDisk, f.AssignedDisk) || d.Size != f.Size {
			return changed("identity or size", d.Device)
		}
	}
	if p.Cache != nil && (!sameAssigned(p.Cache.AssignedDisk, fresh.Cache.AssignedDisk) || p.Cache.Size != fresh.Cache.Size) {
		return changed("identity or size", p.Cache.Device)
	}
	return nil
}

// CheckData runs the read-only filesystem check of Q23 on every data disk, each
// through the identity-bound path it will be mounted by, before anything is
// mounted. The first failure refuses the whole plan. Parity and cache disks are
// never opened.
func (p AdoptionPlan) CheckData(ctx context.Context, r Runner) error {
	for _, d := range p.Data {
		if err := AdoptCheck(ctx, r, d.MountSourceOrNode(), d.Filesystem); err != nil {
			return err
		}
	}
	return nil
}

// MountUnits returns the read-only mount unit of each data disk, at
// /mnt/diskN in plan order, bound to the disk's own device when it has an
// identity to bind to.
func (p AdoptionPlan) MountUnits() []MountUnit {
	units := make([]MountUnit, 0, len(p.Data))
	for i, d := range p.Data {
		units = append(units, MountUnit{
			Where:       fmt.Sprintf("/mnt/disk%d", i+1),
			UUID:        d.FSUUID,
			Filesystem:  d.Filesystem,
			Description: fmt.Sprintf("Hoserva data disk %d", i+1),
			ReadOnly:    true,
			What:        d.MountSource,
		})
	}
	return units
}
