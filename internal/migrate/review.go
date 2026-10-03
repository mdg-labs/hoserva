package migrate

import (
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// Review is what the Review step of the migration needs as data, not prose. It
// is built from the same scan results as the report's rows, in the same pass,
// and is kept in the session with the report. A report made before it existed
// has none, and the API then says so by omitting it.
type Review struct {
	Disks   []ReviewDisk   `json:"disks"`
	Shares  []SharePreview `json:"shares"`
	Boot    ReviewBoot     `json:"boot"`
	Capture CaptureReview  `json:"capture"`
}

// UnraidRole is the role the capture records for a disk.
type UnraidRole string

const (
	UnraidParity     UnraidRole = "parity"
	UnraidData       UnraidRole = "data"
	UnraidCache      UnraidRole = "cache"
	UnraidBoot       UnraidRole = "boot"
	UnraidUnassigned UnraidRole = "unassigned"
)

// ProposedRole is the Hoserva role the import pre-fills for a disk.
type ProposedRole string

const (
	ProposeParity ProposedRole = "parity"
	ProposeData   ProposedRole = "data"
	ProposeCache  ProposedRole = "cache"
	ProposeIgnore ProposedRole = "ignore"
)

// RefusalCode says why the scan refused a disk, beside the report's prose.
type RefusalCode string

const (
	RefuseBootDevice         RefusalCode = "boot_device"
	RefuseHostBoot           RefusalCode = "host_boot"
	RefuseFailed             RefusalCode = "failed"
	RefuseEncrypted          RefusalCode = "encrypted"
	RefuseZFS                RefusalCode = "zfs"
	RefuseUnsupportedFS      RefusalCode = "unsupported_filesystem"
	RefuseFilesystemClash    RefusalCode = "filesystem_mismatch"
	RefuseNoFilesystem       RefusalCode = "no_filesystem"
	RefuseNoFilesystemNode   RefusalCode = "no_filesystem_node"
	RefuseDuplicateUUID      RefusalCode = "duplicate_uuid"
	RefuseMultiDeviceBtrfs   RefusalCode = "multi_device_btrfs"
	RefuseUnverifiedFS       RefusalCode = "filesystem_unverified"
	RefusePendingLog         RefusalCode = "pending_log"
	RefuseIntegrity          RefusalCode = "integrity_check"
	RefuseUnreadable         RefusalCode = "unreadable"
	RefuseWeakIdentityParity RefusalCode = "weak_identity_parity"
)

// ReviewDisk is one row of the disk mapping table: a disk the capture names, an
// Unraid boot device, or a disk of this machine the capture does not name.
// Absent identity, size and filesystem are unknown, never zero or none.
type ReviewDisk struct {
	// Slot is the capture's slot or pool ("disk1", "parity", "pool cache"), or
	// "boot" for a boot device; empty for a disk the capture does not name.
	Slot string `json:"slot,omitempty"`
	// DiskNumber is the Unraid disk number of a data slot.
	DiskNumber int `json:"diskNumber,omitempty"`
	// UnraidID is the identity Unraid recorded for the slot.
	UnraidID string `json:"unraidId,omitempty"`
	// UnraidRole is empty when the capture has no disks.ini, which is the only
	// thing that says whether an unnamed disk was unassigned.
	UnraidRole UnraidRole `json:"unraidRole,omitempty"`
	// ProposedRole is empty when nothing is proposed: no disk of this machine
	// matched the slot, the disk is refused, or the capture has no disks.ini.
	ProposedRole ProposedRole `json:"proposedRole,omitempty"`
	// UnraidBoot is set on the row of a disk that is also an Unraid boot device,
	// whatever slot the row is for: the pool-cache row of an internal boot that
	// shares its disk with the cache, or a parity or data slot that names a boot
	// device (refused, proposed ignore). Such a disk is one row, never a second
	// boot row beside it. Only the cache row of it is proposed a role but ignore.
	UnraidBoot bool `json:"unraidBoot,omitempty"`
	// Device is this machine's device for the disk, empty when none matched.
	Device     string `json:"device,omitempty"`
	Serial     string `json:"serial,omitempty"`
	WWN        string `json:"wwn,omitempty"`
	ByID       string `json:"byId,omitempty"`
	Model      string `json:"model,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Filesystem string `json:"filesystem,omitempty"`
	// WeakIdentity is nil when no disk of this machine matched.
	WeakIdentity *bool `json:"weakIdentity,omitempty"`
	// Problem says why no disk of this machine matched a slot.
	Problem     string      `json:"problem,omitempty"`
	Refused     bool        `json:"refused"`
	RefusalCode RefusalCode `json:"refusalCode,omitempty"`
	Refusal     string      `json:"refusal,omitempty"`
}

func (d *ReviewDisk) setIdentity(m *disk.Disk) {
	weak := m.WeakIdentity
	d.Device, d.Serial, d.WWN, d.ByID, d.Model = m.Device, m.Serial, m.WWN, m.ByIDName, m.Model
	d.Size, d.Filesystem, d.WeakIdentity = m.Size, m.Filesystem, &weak
}

// SharePreview is one share the import would create, from the same config its
// report row reads. Include and Exclude are the disks the share is limited to or
// kept off; both empty means any disk.
type SharePreview struct {
	Name string `json:"name"`
	// AllocationMethod is Unraid's own value (fillup, mostfree, highwater);
	// empty when the config sets none.
	AllocationMethod string   `json:"allocationMethod,omitempty"`
	HighWater        bool     `json:"highWater"`
	Include          []string `json:"include"`
	Exclude          []string `json:"exclude"`
	// WarningCount is how many things the share's report row flags or warns
	// about.
	WarningCount int `json:"warningCount"`
}

// ReviewBoot is where Unraid boots from. Mode is empty when the capture does not
// say, or says something other than usb or internal, and Mirrored and
// SharedWithCache are set only for an internal boot.
type ReviewBoot struct {
	Mode            string `json:"mode,omitempty"`
	Mirrored        *bool  `json:"mirrored,omitempty"`
	SharedWithCache *bool  `json:"sharedWithCache,omitempty"`
}

// CaptureState says what the scan found of the Phase A capture.
type CaptureState string

const (
	CapturePresent    CaptureState = "present"
	CaptureMissing    CaptureState = "missing"
	CaptureUnreadable CaptureState = "unreadable"
	CaptureStale      CaptureState = "stale"
)

// CaptureReview is the capture's state. CapturedAt is nil when the capture
// states no time or one that is not a timestamp; a present capture with no
// CapturedAt was not checked for staleness.
type CaptureReview struct {
	State      CaptureState `json:"state"`
	CapturedAt *time.Time   `json:"capturedAt,omitempty"`
}

func (f *Flash) capturedAt() (time.Time, bool) {
	if f.Capture == nil || f.Capture.CapturedAt == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, f.Capture.CapturedAt)
	if err != nil {
		return time.Time{}, false
	}
	return at.UTC(), true
}

// ReviewCapture decides the capture's state. The capture is stale when a
// template on the flash was saved after it was taken.
func (f *Flash) ReviewCapture() CaptureReview {
	switch {
	case f.CaptureProblem != "":
		return CaptureReview{State: CaptureUnreadable}
	case f.Capture == nil:
		return CaptureReview{State: CaptureMissing}
	}
	at, ok := f.capturedAt()
	if !ok {
		return CaptureReview{State: CapturePresent}
	}
	out := CaptureReview{State: CapturePresent, CapturedAt: &at}
	if !f.TemplatesSavedAt.IsZero() && at.Before(f.TemplatesSavedAt) {
		out.State = CaptureStale
	}
	return out
}

// ReviewBoot reads the boot mode and layout from the capture.
func (f *Flash) ReviewBoot() ReviewBoot {
	if f.Capture == nil {
		return ReviewBoot{}
	}
	b := f.Capture.Boot
	switch b.Mode {
	case "usb":
		return ReviewBoot{Mode: b.Mode}
	case "internal":
		mirrored, shared := b.Mirrored, b.SharedWithDataPool
		return ReviewBoot{Mode: b.Mode, Mirrored: &mirrored, SharedWithCache: &shared}
	}
	return ReviewBoot{}
}

// BootDisks are the Unraid boot devices as rows of the mapping table: the
// devices the capture names, matched to this machine's disks by serial, and
// every disk of this machine that is an Unraid boot device. A boot device is
// never given a role but ignore.
func BootDisks(f *Flash, machine []disk.Disk) []ReviewDisk {
	var out []ReviewDisk
	taken := map[string]bool{}
	row := func() ReviewDisk {
		return ReviewDisk{Slot: "boot", UnraidRole: UnraidBoot, ProposedRole: ProposeIgnore}
	}
	if f.Capture != nil {
		for _, bd := range f.Capture.Boot.Devices {
			rd := row()
			rd.Serial, rd.Model = bd.Serial, bd.Model
			var here []*disk.Disk
			for i := range machine {
				if bd.Serial != "" && strings.EqualFold(machine[i].Serial, bd.Serial) {
					here = append(here, &machine[i])
				}
			}
			if len(here) == 1 {
				rd.setIdentity(here[0])
				taken[here[0].Device] = true
			}
			out = append(out, rd)
		}
	}
	for i := range machine {
		if d := &machine[i]; (d.UnraidBoot || disk.IsUnraidStick(*d)) && !taken[d.Device] {
			rd := row()
			rd.setIdentity(d)
			out = append(out, rd)
		}
	}
	return out
}

// AddBootDisks adds the Unraid boot devices to rows, the table's rows for the
// slots the capture names. A boot device that is already a row, as the pool of
// an internal boot that shares its disk with the cache, is not a second row: that
// row is marked UnraidBoot instead.
func AddBootDisks(rows []ReviewDisk, f *Flash, machine []disk.Disk) []ReviewDisk {
	rowOf := map[string]int{}
	for i, d := range rows {
		if d.Device != "" {
			if _, dup := rowOf[d.Device]; !dup {
				rowOf[d.Device] = i
			}
		}
	}
	for _, bd := range BootDisks(f, machine) {
		if i, ok := rowOf[bd.Device]; ok && bd.Device != "" {
			rows[i].UnraidBoot = true
			continue
		}
		rows = append(rows, bd)
		if bd.Device != "" {
			rowOf[bd.Device] = len(rows) - 1
		}
	}
	return rows
}

// reviewDisks builds the mapping table. Each disk of this machine is in it
// once: as the slot that names it (marked UnraidBoot when it is also an Unraid
// boot device), as a boot device, or as an unnamed disk. A disk of this machine
// that matches any slot's identity is never listed as unnamed, even when the
// match was ambiguous and no slot took it, and this machine's own boot disk is
// listed only as the slot that names it: a parity or data slot is refused or
// proposed nothing, and the cache pool's is proposed cache (the shared NVMe of
// doc 01 §6 and doc 05 §4).
func reviewDisks(f *Flash, members []*member, machine []disk.Disk) []ReviewDisk {
	out := []ReviewDisk{}
	for _, m := range members {
		out = append(out, m.reviewDisk(f))
	}
	out = AddBootDisks(out, f, machine)
	claimed := map[string]bool{}
	for _, d := range out {
		if d.Device != "" {
			claimed[d.Device] = true
		}
	}
	for i := range machine {
		d := &machine[i]
		if claimed[d.Device] || d.Boot || claimedByAny(members, *d) {
			continue
		}
		rd := ReviewDisk{}
		rd.setIdentity(d)
		if f.HaveDisksINI {
			rd.UnraidRole = UnraidUnassigned
		}
		out = append(out, rd)
	}
	return out
}

func claimedByAny(members []*member, d disk.Disk) bool {
	for _, m := range members {
		if idMatches(m.id, d) {
			return true
		}
	}
	return false
}

func (m *member) reviewDisk(f *Flash) ReviewDisk {
	rd := ReviewDisk{Slot: m.subject, UnraidID: m.id}
	switch m.role {
	case RoleParity:
		rd.UnraidRole = UnraidParity
	case RoleData:
		rd.UnraidRole, rd.DiskNumber = UnraidData, m.index
	case RoleCache:
		rd.UnraidRole = UnraidCache
	}
	if m.disk != nil {
		rd.setIdentity(m.disk)
	} else {
		rd.Problem = m.problem
		rd.Size = m.unraid
	}
	if m.refusalCode != "" {
		rd.Refused, rd.RefusalCode, rd.Refusal = true, m.refusalCode, m.refusalText
	}
	switch {
	case m.refusalCode == RefuseBootDevice:
		rd.ProposedRole = ProposeIgnore
	case m.refusalCode != "", m.disk == nil:
	case m.disk.Boot && m.role != RoleCache:
	case m.role != RoleCache && (m.disk.UnraidBoot || disk.IsUnraidStick(*m.disk)):
		rd.ProposedRole = ProposeIgnore
	case !f.HaveDisksINI:
	case m.role == RoleParity:
		rd.ProposedRole = ProposeParity
	case m.role == RoleData:
		rd.ProposedRole = ProposeData
	case m.role == RoleCache:
		rd.ProposedRole = ProposeCache
	}
	return rd
}
