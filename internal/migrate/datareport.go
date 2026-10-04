package migrate

import (
	"fmt"
	"sort"
	"strings"
)

// describeBaseline adds the baseline's rows to the report: the sample rule, each
// disk's totals, each share's totals across the disks, and the paths several
// disks hold.
func describeBaseline(r *Report, b *BaselineSummary) {
	r.add(CheckBaseline, StatusInfo, "", "Recorded for the verify phase, with the session. Content hashes: %s. Every file's size, every symlink with its target and every special file by type is recorded as well.", b.Rule.Describe())
	for _, d := range b.Disks {
		line := fmt.Sprintf("%s (%s), %s and %s; %s (%s) hashed.", count(d.Files, "file", "files"), formatBytes(d.Bytes), count(d.Symlinks, "symlink", "symlinks"), count(d.Special, "special file", "special files"), count(d.Hashed, "file", "files"), formatBytes(d.HashedBytes))
		if d.RootEntries > 0 {
			line += fmt.Sprintf(" %d entries sit directly in the disk's root, outside any share; they are recorded under no share.", d.RootEntries)
		}
		if len(d.HiddenSkipped) > 0 {
			line += fmt.Sprintf(" Hidden top-level directories are not shares and are left out: %s.", joinNames(d.HiddenSkipped))
		}
		r.add(CheckBaseline, StatusInfo, d.Slot, "%s", line)
	}
	type total struct {
		ShareTotals
		disks []string
	}
	byShare := map[string]*total{}
	for _, d := range b.Disks {
		for _, s := range d.Shares {
			if s.Share == "" {
				continue
			}
			t := byShare[s.Share]
			if t == nil {
				t = &total{ShareTotals: ShareTotals{Share: s.Share}}
				byShare[s.Share] = t
			}
			t.Files += s.Files
			t.Symlinks += s.Symlinks
			t.Special += s.Special
			t.Bytes += s.Bytes
			t.disks = append(t.disks, d.Slot)
		}
	}
	names := make([]string, 0, len(byShare))
	for n := range byShare {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t := byShare[n]
		r.add(CheckBaseline, StatusInfo, n, "%s (%s), %s and %s, on %s.", count(t.Files, "file", "files"), formatBytes(t.Bytes), count(t.Symlinks, "symlink", "symlinks"), count(t.Special, "special file", "special files"), joinNames(t.disks))
	}
	if b.Duplicates > 0 {
		var parts []string
		for _, d := range b.DuplicateSample {
			parts = append(parts, fmt.Sprintf("%s (on %s)", d.Path, joinNames(d.Disks)))
		}
		more := ""
		if b.Duplicates > int64(len(b.DuplicateSample)) {
			more = fmt.Sprintf(" The first %d are listed; the baseline records all of them.", len(b.DuplicateSample))
		}
		r.add(CheckBaseline, StatusFlag, "", "%s on more than one disk, and the pool shows only one copy of each (mergerfs, as Unraid's user shares do): %s.%s Decide which copy to keep before relying on the pool.", count(b.Duplicates, "path exists", "paths exist"), strings.Join(parts, "; "), more)
	}
}

// count says "1 file" or "2 files".
func count(n int64, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Planning figures for the size of SnapRAID's content file: a per-file entry and
// a per-block hash. They set expectations, they are not measurements.
const (
	contentBytesPerFile  = 200
	contentBytesPerBlock = 24
	contentBlockSize     = 256 * 1024
)

func estimateContentBytes(files, bytes int64) int64 {
	return files*contentBytesPerFile + bytes/contentBlockSize*contentBytesPerBlock
}

// checkContentSpace reports whether doc 02 §2's content-file placement is
// possible after the migration (Q18): copies on the boot device, on the cache when
// it is a device of its own, and on data disks, at least the parity disks + 2,
// each with room for the file. It needs the baseline's file count for its
// estimate, so it says so when no data disk was read.
func (s *Scanner) checkContentSpace(r *Report, f *Flash, members []*member) {
	var data []*member
	cache := false
	for _, m := range members {
		switch m.role {
		case RoleData:
			data = append(data, m)
		case RoleCache:
			if m.disk != nil && !m.disk.Boot && !m.disk.UnraidBoot {
				cache = true
			}
		}
	}
	if len(data) == 0 {
		return
	}
	if r.Baseline == nil {
		r.add(CheckContentSpace, StatusInfo, "", "Not evaluated: no data disk was read, so the number of files, and the size of the content file they need, is not known.")
		return
	}
	var files, bytes int64
	for _, d := range r.Baseline.Disks {
		files += d.Files
		bytes += d.Bytes
	}
	need := parityCount(f) + 2
	estimate := estimateContentBytes(files, bytes)
	dataCopies := need - 1
	if cache {
		dataCopies--
	}
	dataCopies = max(dataCopies, 0)

	var withRoom, adopted int
	for _, m := range data {
		if m.fs == "" || m.refusal != "" {
			continue
		}
		adopted++
		if m.freeKnown && m.free >= estimate {
			withRoom++
		}
	}
	places := []string{"the boot device"}
	if cache {
		places = append(places, "the cache")
	}
	places = append(places, fmt.Sprintf("%d data %s", dataCopies, plural(dataCopies, "disk", "disks")))
	where := strings.Join(places, ", ")
	figure := fmt.Sprintf("about %s for the content file (%d files, a planning figure of %d bytes per file and %d per 256 KiB block)", formatBytes(estimate), files, contentBytesPerFile, contentBytesPerBlock)

	bootFree, bootErr := int64(0), error(nil)
	if s.Dir != "" {
		bootFree, bootErr = s.free(s.Dir)
	}
	switch {
	case adopted < dataCopies:
		r.add(CheckContentSpace, StatusFlag, "", "Q18 wants %d content-file copies (parity disks + 2) on %s, and only %d data disks will be adopted. The import refuses a layout that cannot place them: add or keep more data disks, or give the import a cache device of its own.", need, where, adopted)
	case withRoom < dataCopies:
		r.add(CheckContentSpace, StatusFlag, "", "Q18 wants %d content-file copies on %s, but only %d of the adopted data disks have room for %s.", need, where, withRoom, figure)
	case s.Dir == "" || bootErr != nil:
		r.add(CheckContentSpace, StatusWarn, "", "The data disks can hold the %d content-file copies (%s), but the free space on this machine's boot device could not be read, and it holds the first copy.", need, figure)
	case bootFree < estimate:
		r.add(CheckContentSpace, StatusFlag, "", "The boot device has %s free, and it holds the first content-file copy, which needs %s.", formatBytes(bootFree), figure)
	default:
		r.add(CheckContentSpace, StatusPass, "", "%d content-file copies can be placed (Q18): %s, with room for %s.", need, where, figure)
	}
}

// parityCount is how many parity disks the configuration assigns.
func parityCount(f *Flash) int {
	n := 0
	if f.HaveDisksINI {
		for _, sl := range f.Slots {
			if sl.Role() == RoleParity && sl.Assigned() {
				n++
			}
		}
	} else {
		n = len(f.DiskCfg.ParitySlots)
	}
	return max(n, 1)
}
