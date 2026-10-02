package disk

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// linuxDataPartitionTypes are the partition-table type identifiers a
// spare cache partition may carry: the GPT "Linux filesystem data" GUID
// and the MBR 0x83 type. Swap, EFI system, BIOS boot and every other
// type never qualify, whatever else is true of the partition.
var linuxDataPartitionTypes = []string{"0fc63daf-8483-4772-8e79-3d69d8477de4", "0x83"}

// systemdMountUnitDirs are the directories whose .mount and .swap units
// can name a partition as their What=.
var systemdMountUnitDirs = []string{
	"/etc/systemd/system", "/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system",
}

// cacheCandidates returns the boot disk's partitions that qualify to hold
// the cache (doc 01 §6, doc 02 §4): same-disk, Linux-data-typed,
// filesystem-signature-free in udev's cache, not mounted, not swap, not named in
// /etc/fstab or any systemd mount or swap unit, held open by nothing, and
// carrying both a by-id link built from the disk's own and a PARTUUID —
// the identity everything after this binds to. It reads only sysfs, udev's
// database, by-id, mounts, swaps, fstab and unit files; it never opens a
// device, so it never wakes a disk. Every source it cannot read, and an
// unset swaps or fstab path, yields no candidates at all: the exclusions
// are what keep the root, EFI and swap partitions out, so a source that
// cannot be consulted must never be treated as empty.
func (l *Lister) cacheCandidates(parentName string, parent Identity, mounts []MountEntry) []CachePartition {
	if l.SwapsFile == "" || l.FstabFile == "" || parent.ByIDName == "" {
		return nil
	}
	named, err := l.namedPartitionSpecs(mounts)
	if err != nil {
		return nil
	}
	byID, err := scanPartitionByID(l.ByIDDir)
	if err != nil {
		return nil
	}

	var out []CachePartition
	for _, part := range l.partitions(parentName) {
		if c, ok := l.cacheCandidate(part, parent, byID[part], named); ok {
			out = append(out, c)
		}
	}
	return out
}

// bootPartitions lists every partition of the boot disk with the
// identity and filesystem udev's cache holds for it, in partition-number
// order. Unlike cacheCandidates it applies no exclusion: a formatted, mounted
// or swap partition is listed too, since a bare-metal restore has to see a
// formatted cache partition to recognise it. It reads sysfs, udev's
// database and by-id only and never opens a device. A partition udev has no
// entry for is listed with no filesystem, and an unreadable by-id directory
// leaves every ByIDName empty: a caller that cannot see what it needs then
// finds nothing to match, never a partition it could mistake for another.
func (l *Lister) bootPartitions(parentName string, parent Identity) []BootPartition {
	byID, _ := scanPartitionByID(l.ByIDDir)
	var out []BootPartition
	for _, part := range l.partitions(parentName) {
		p := BootPartition{Device: "/dev/" + part}
		if sectors, err := readSysInt64(filepath.Join(l.SysBlockDir, part, "size")); err == nil && sectors > 0 {
			p.Size = sectors * 512
		}
		if number := strings.TrimSpace(readSysString(filepath.Join(l.SysBlockDir, part, "partition"))); number != "" && parent.ByIDName != "" {
			if want := parent.ByIDName + "-part" + number; slices.Contains(byID[part], want) {
				p.ByIDName = want
			}
		}
		if props, ok := l.udevProps(part); ok {
			p.PartUUID = props["ID_PART_ENTRY_UUID"]
			p.Filesystem = props["ID_FS_TYPE"]
			p.FSUUID = props["ID_FS_UUID"]
		}
		out = append(out, p)
	}
	return out
}

func (l *Lister) cacheCandidate(part string, parent Identity, byIDNames, named []string) (CachePartition, bool) {
	number := strings.TrimSpace(readSysString(filepath.Join(l.SysBlockDir, part, "partition")))
	wantByID := parent.ByIDName + "-part" + number
	if number == "" || !slices.Contains(byIDNames, wantByID) {
		return CachePartition{}, false
	}
	sectors, err := readSysInt64(filepath.Join(l.SysBlockDir, part, "size"))
	if err != nil || sectors <= 0 {
		return CachePartition{}, false
	}
	props, ok := l.udevProps(part)
	if !ok {
		return CachePartition{}, false
	}
	for key := range props {
		if strings.HasPrefix(key, "ID_FS_") {
			return CachePartition{}, false
		}
	}
	if !slices.Contains(linuxDataPartitionTypes, strings.ToLower(props["ID_PART_ENTRY_TYPE"])) {
		return CachePartition{}, false
	}
	partUUID := props["ID_PART_ENTRY_UUID"]
	if partUUID == "" {
		return CachePartition{}, false
	}
	holders, err := os.ReadDir(filepath.Join(l.SysBlockDir, part, "holders"))
	if err != nil || len(holders) > 0 {
		return CachePartition{}, false
	}
	dev := "/dev/" + part
	for _, spec := range named {
		if specNamesPartition(spec, dev, partUUID, props["ID_PART_ENTRY_NAME"], byIDNames) {
			return CachePartition{}, false
		}
	}
	return CachePartition{
		Device:   dev,
		Size:     sectors * 512,
		ByIDName: wantByID,
		PartUUID: partUUID,
		Reason:   ReasonSpareBootPartition,
	}, true
}

// namedPartitionSpecs collects every device specification that something
// already claims: the mounts listing, /proc/swaps, /etc/fstab and the
// What= of every systemd .mount and .swap unit. An unreadable source is
// an error; a unit directory that does not exist is not.
func (l *Lister) namedPartitionSpecs(mounts []MountEntry) ([]string, error) {
	var specs []string
	for _, m := range mounts {
		specs = append(specs, m.Device)
	}
	swaps, err := firstFields(l.SwapsFile, "Filename")
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", l.SwapsFile, err)
	}
	specs = append(specs, swaps...)
	fstab, err := firstFields(l.FstabFile, "")
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", l.FstabFile, err)
	}
	specs = append(specs, fstab...)
	for _, dir := range l.MountUnitDirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", dir, err)
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".mount") && !strings.HasSuffix(e.Name(), ".swap") {
				continue
			}
			whats, err := unitWhats(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			specs = append(specs, whats...)
		}
	}
	return specs, nil
}

// firstFields returns the first whitespace-separated field of every
// non-blank, non-comment line of path, skipping a line that starts with
// header (the /proc/swaps column header).
func firstFields(path, header string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") || (header != "" && fields[0] == header) {
			continue
		}
		out = append(out, fields[0])
	}
	return out, sc.Err()
}

func unitWhats(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "What="); ok {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out, nil
}

// specNamesPartition reports whether a device specification from a
// mounts listing, swaps, fstab or unit file names the partition at dev:
// by kernel path, PARTUUID, PARTLABEL, ID (a /dev/disk/by-id name), a
// /dev/disk/by-partuuid, by-partlabel or by-id link, or any other /dev path
// that resolves to it.
// A label is compared with partLabel, the partition's udev
// ID_PART_ENTRY_NAME; when udev reports none, or the name cannot be
// decoded, the spec cannot be ruled out, so it counts as naming the
// partition. A PARTLABEL=, PARTUUID= or ID= value may be wrapped in one pair of
// matching quotes, the way blkid prints it; an unbalanced quote leaves the
// value unreadable, so it counts as naming the partition too.
func specNamesPartition(spec, dev, partUUID, partLabel string, byIDNames []string) bool {
	spec = strings.TrimSpace(spec)
	if label, ok := strings.CutPrefix(spec, "PARTLABEL="); ok {
		label, balanced := unquoteTagValue(label)
		return !balanced || partLabelMayMatch(label, partLabel)
	}
	if id, ok := strings.CutPrefix(spec, "PARTUUID="); ok {
		id, balanced := unquoteTagValue(id)
		return !balanced || strings.EqualFold(id, partUUID)
	}
	if id, ok := strings.CutPrefix(spec, "ID="); ok {
		id, balanced := unquoteTagValue(id)
		return !balanced || slices.Contains(byIDNames, id)
	}
	if label, ok := strings.CutPrefix(spec, "/dev/disk/by-partlabel/"); ok && partLabelMayMatch(label, partLabel) {
		return true
	}
	switch {
	case spec == dev:
		return true
	case strings.EqualFold(spec, "/dev/disk/by-partuuid/"+partUUID):
		return true
	case strings.HasPrefix(spec, "/dev/disk/by-id/") && slices.Contains(byIDNames, filepath.Base(spec)):
		return true
	}
	if strings.HasPrefix(spec, "/dev/") {
		for _, path := range []string{spec, unescape(spec, true, false)} {
			if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved == dev {
				return true
			}
		}
	}
	return false
}

// unquoteTagValue strips one pair of matching single or double quotes from
// a tag value. It reports false for a value with a quote on only one end,
// or with two different ones.
func unquoteTagValue(v string) (string, bool) {
	isQuote := func(c byte) bool { return c == '"' || c == '\'' }
	if v == "" {
		return v, true
	}
	first, last := v[0], v[len(v)-1]
	switch {
	case len(v) >= 2 && isQuote(first) && first == last:
		return v[1 : len(v)-1], true
	case isQuote(first) || isQuote(last):
		return v, false
	}
	return v, true
}

// partLabelMayMatch reports whether a PARTLABEL or by-partlabel name can
// be the partition label udev reports as ID_PART_ENTRY_NAME. udev writes
// that property with every unsafe byte as \xNN (blkid_encode_string), so
// both sides are decoded to raw bytes first: the udev value's \xNN
// escapes, and the spec's fstab octal (\040) escapes, with and without
// the \xNN escapes a by-partlabel link name or a systemd unit's What=
// carries. An empty or undecodable udev name never rules a spec out.
func partLabelMayMatch(specLabel, udevName string) bool {
	if udevName == "" || hasBareBackslash(udevName) {
		return true
	}
	want := unescape(udevName, false, true)
	octal := unescape(specLabel, true, false)
	return octal == want || unescape(octal, false, true) == want
}

// hasBareBackslash reports a backslash in a udev-encoded name that does
// not start a \xNN escape; udev encodes every backslash itself as \x5c.
func hasBareBackslash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			continue
		}
		if i+3 < len(s) && s[i+1] == 'x' && isHex(s[i+2]) && isHex(s[i+3]) {
			i += 3
			continue
		}
		return true
	}
	return false
}

// unescape decodes fstab octal (\NNN) and/or \xNN escapes to raw bytes in
// one pass; anything else, including a malformed escape, is kept as is.
func unescape(s string, octal, hex bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			if octal && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
				b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
				i += 3
				continue
			}
			if hex && i+3 < len(s) && s[i+1] == 'x' && isHex(s[i+2]) && isHex(s[i+3]) {
				v, _ := strconv.ParseUint(s[i+2:i+4], 16, 8)
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// scanPartitionByID is scanByID for the "-partN" entries it skips: every
// partition's kernel name mapped to every by-id link naming it.
func scanPartitionByID(dir string) (map[string][]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string)
	for _, e := range entries {
		name := e.Name()
		if !partitionByIDSuffix.MatchString(name) {
			continue
		}
		target, err := os.Readlink(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		part := filepath.Base(target)
		out[part] = append(out[part], name)
	}
	return out, nil
}
