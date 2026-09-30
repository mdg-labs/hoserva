package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

// The states a recorded array disk can have against the attached disks.
const (
	DiskMatched   = "matched"
	DiskAbsent    = "absent"
	DiskReplaced  = "replaced"
	DiskAmbiguous = "ambiguous"
)

// MappedDisk is one array disk an archive records and what a bare-metal
// restore found for it among the attached disks. Attached is the disk it
// matched, or, for a replaced disk, the disk that holds its filesystem or
// carries its identity; it is nil for an absent or ambiguous one.
type MappedDisk struct {
	Recorded store.ArrayDisk
	State    string
	Attached *disk.Disk
}

// Name is what a user calls the disk: its slot, its mountpoint and the
// identity it was recorded with.
func (m MappedDisk) Name() string {
	r := m.Recorded
	name := fmt.Sprintf("%s disk %d (%s", r.Role, r.RoleIndex, r.Mountpoint)
	switch {
	case r.WWN != "":
		name += ", WWN " + r.WWN
	case r.Serial != "":
		name += ", serial " + r.Serial
	default:
		name += ", filesystem " + r.FSUUID
	}
	return name + ")"
}

// MatchAttachedDisk finds the recorded array_disks row that identifies the
// same physical disk as d (Q21): disk.Identity.Matches, WWN when both sides
// have one, else serial. It never falls back to comparing /dev/sdX paths,
// which renumber across reboots (#326). A weak-identity row (no wwn/serial
// by-id link at all, as with every disk in the loop-device lab, doc 06 §3)
// is matched by filesystem UUID and size instead, since disk.Identity has
// no field to compare those through Matches; a row with no recorded size
// (NULL, from before #327) falls back to filesystem UUID alone.
func MatchAttachedDisk(d disk.Disk, recorded []store.ArrayDisk) (int, bool) {
	inv := disk.Identity{WWN: d.WWN, Serial: d.Serial, WeakIdentity: d.WeakIdentity, ByIDName: d.ByIDName}
	for i, ad := range recorded {
		stored := disk.Identity{WWN: ad.WWN, Serial: ad.Serial, WeakIdentity: ad.WeakIdentity, ByIDName: ad.ByIDName}
		if inv.Matches(stored) {
			return i, true
		}
		if ad.WeakIdentity && d.WeakIdentity && ad.FSUUID != "" && ad.FSUUID == d.FSUUID {
			if ad.SizeSet && ad.Size != d.Size {
				continue
			}
			return i, true
		}
	}
	return 0, false
}

// MapArrayDisks matches every recorded array disk against the attached
// disks with MatchAttachedDisk, the match GetPool makes for the live array,
// and reports each in the order given. A recorded disk claimed by more than
// one attached disk (a disk and its clone) is ambiguous and is assigned to
// neither. A recorded disk no attached disk claims is replaced when a
// disk that no recorded disk claims holds its filesystem, and absent
// otherwise; one whose identity matches but whose filesystem differs is
// replaced too (disk.FSUUIDMismatch). The boot disk is never a candidate.
// It reads the given lists only.
func MapArrayDisks(recorded []store.ArrayDisk, attached []disk.Disk) []MappedDisk {
	var present []disk.Disk
	for _, d := range attached {
		if !d.Boot {
			present = append(present, d)
		}
	}
	claimedBy := make([]int, len(present))
	claimants := make([]int, len(recorded))
	for i, d := range present {
		claimedBy[i] = -1
		if idx, ok := MatchAttachedDisk(d, recorded); ok {
			claimedBy[i] = idx
			claimants[idx]++
		}
	}

	out := make([]MappedDisk, len(recorded))
	for r, rec := range recorded {
		out[r] = MappedDisk{Recorded: rec, State: DiskAbsent}
		switch {
		case claimants[r] > 1:
			out[r].State = DiskAmbiguous
		case claimants[r] == 1:
			i := slices.Index(claimedBy, r)
			d := present[i]
			out[r].Attached = &d
			out[r].State = DiskMatched
			if disk.FSUUIDMismatch(rec.FSUUID, d.FSUUID) {
				out[r].State = DiskReplaced
			}
		case rec.FSUUID != "":
			for i, d := range present {
				if claimedBy[i] == -1 && d.FSUUID == rec.FSUUID {
					out[r].Attached = &d
					out[r].State = DiskReplaced
					break
				}
			}
		}
	}
	return out
}

// DiskMappingEntry is one slot of a confirmed disk mapping: the attached
// device the user confirmed the archive's disk in that slot is.
type DiskMappingEntry struct {
	Role      string
	RoleIndex int
	Device    string
}

// DiskMapping is the mapping a user confirmed: an entry for each disk that
// matched, and none for any other.
type DiskMapping struct {
	Disks []DiskMappingEntry
}

// ErrInvalidDiskMapping is returned by ParseDiskMapping for a document that
// is not a disk mapping.
var ErrInvalidDiskMapping = errors.New("not a valid disk mapping")

// maxDiskMappingEntries bounds a parsed mapping: an array has a handful of
// disks, and the API schema allows the same number.
const maxDiskMappingEntries = 512

// ParseDiskMapping reads the JSON of a confirmed mapping, the API's
// ConfigImportDiskMapping: {"disks": [{"role", "roleIndex", "device"}, ...]}.
// It refuses anything else, an unknown field, a role that is not parity, data
// or cache, a role index below 1, an empty device, or trailing content, as
// ErrInvalidDiskMapping, so a mapping is never half read.
func ParseDiskMapping(doc string) (DiskMapping, error) {
	var raw struct {
		Disks *[]struct {
			Role      string `json:"role"`
			RoleIndex int    `json:"roleIndex"`
			Device    string `json:"device"`
		} `json:"disks"`
	}
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return DiskMapping{}, fmt.Errorf("%w: %v", ErrInvalidDiskMapping, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return DiskMapping{}, fmt.Errorf("%w: content after the mapping", ErrInvalidDiskMapping)
	}
	if raw.Disks == nil {
		return DiskMapping{}, fmt.Errorf("%w: no disks list", ErrInvalidDiskMapping)
	}
	if len(*raw.Disks) > maxDiskMappingEntries {
		return DiskMapping{}, fmt.Errorf("%w: more than %d disks", ErrInvalidDiskMapping, maxDiskMappingEntries)
	}
	m := DiskMapping{Disks: make([]DiskMappingEntry, len(*raw.Disks))}
	for i, e := range *raw.Disks {
		switch {
		case !slices.Contains([]string{store.ArrayRoleParity, store.ArrayRoleData, store.ArrayRoleCache}, e.Role):
			return DiskMapping{}, fmt.Errorf("%w: disk %d has role %q", ErrInvalidDiskMapping, i+1, e.Role)
		case e.RoleIndex < 1:
			return DiskMapping{}, fmt.Errorf("%w: disk %d has role index %d", ErrInvalidDiskMapping, i+1, e.RoleIndex)
		case e.Device == "" || len(e.Device) > 256:
			return DiskMapping{}, fmt.Errorf("%w: disk %d has no usable device", ErrInvalidDiskMapping, i+1)
		}
		m.Disks[i] = DiskMappingEntry{Role: e.Role, RoleIndex: e.RoleIndex, Device: e.Device}
	}
	return m, nil
}

// MatchedMapping is the mapping that confirms exactly the matched disks of
// mapped, in their order.
func MatchedMapping(mapped []MappedDisk) DiskMapping {
	m := DiskMapping{Disks: []DiskMappingEntry{}}
	for _, d := range mapped {
		if d.State == DiskMatched {
			m.Disks = append(m.Disks, DiskMappingEntry{Role: d.Recorded.Role, RoleIndex: d.Recorded.RoleIndex, Device: d.Attached.Device})
		}
	}
	return m
}

// ErrDiskMappingRequired is returned when a bare-metal restore was asked for
// without the mapping the user confirmed.
var ErrDiskMappingRequired = errors.New("restoring onto a fresh install needs the disk mapping confirmed: preview the import to see the mapping, then confirm it with the import")

// DiskMappingStaleError is returned when the mapping the user confirmed is
// not the one the attached disks give now; Differences names each.
type DiskMappingStaleError struct{ Differences []string }

func (e *DiskMappingStaleError) Error() string {
	return "the confirmed disk mapping no longer matches the attached disks: " + strings.Join(e.Differences, "; ") + "; preview the import again"
}

// checkConfirmed compares confirmed with the mapping mapped gives now: the
// same slots, each on the same device.
func checkConfirmed(mapped []MappedDisk, confirmed DiskMapping) error {
	type slot struct {
		role  string
		index int
	}
	got := map[slot]string{}
	var diffs []string
	for _, e := range confirmed.Disks {
		s := slot{e.Role, e.RoleIndex}
		if _, dup := got[s]; dup {
			diffs = append(diffs, fmt.Sprintf("%s disk %d is confirmed twice", e.Role, e.RoleIndex))
		}
		got[s] = e.Device
	}
	recorded := map[slot]bool{}
	for _, d := range mapped {
		s := slot{d.Recorded.Role, d.Recorded.RoleIndex}
		recorded[s] = true
		dev, confirmedSlot := got[s]
		if d.State != DiskMatched {
			if confirmedSlot {
				diffs = append(diffs, fmt.Sprintf("%s is confirmed on %s but is %s", d.Name(), dev, d.State))
			}
			continue
		}
		switch {
		case !confirmedSlot:
			diffs = append(diffs, fmt.Sprintf("%s matches %s but was not confirmed", d.Name(), d.Attached.Device))
		case dev != d.Attached.Device:
			diffs = append(diffs, fmt.Sprintf("%s is on %s, not the confirmed %s", d.Name(), d.Attached.Device, dev))
		}
	}
	for _, e := range confirmed.Disks {
		if !recorded[slot{e.Role, e.RoleIndex}] {
			diffs = append(diffs, fmt.Sprintf("%s disk %d is confirmed but the archive records no such disk", e.Role, e.RoleIndex))
		}
	}
	if len(diffs) > 0 {
		return &DiskMappingStaleError{Differences: diffs}
	}
	return nil
}
