package disk

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// unraidLabel matches Unraid's own filesystem labels for array and cache
// members (doc 05): diskN, parity / parity2, cache / cache2. The heuristic
// is label-only — List never mounts a disk to look for super.dat.
var unraidLabel = regexp.MustCompile(`^(?i)(disk[0-9]+|parity[0-9]*|cache[0-9]*)$`)

// LooksLikeUnraidLabel reports whether label is one Unraid applies to an
// array or cache member. Empty labels and unrelated names (media, data)
// are false.
func LooksLikeUnraidLabel(label string) bool {
	return unraidLabel.MatchString(strings.TrimSpace(label))
}

func (l *Lister) udevDataDir() string {
	if l.UdevDataDir != "" {
		return l.UdevDataDir
	}
	return "/run/udev/data"
}

// discoveryFS returns udev-cached ID_FS_TYPE, ID_FS_LABEL and ID_FS_UUID
// for the whole-disk sysfs name, falling back to its partitions when the
// whole disk has none. Primary filesystem rule: the first partition by
// ascending sysfs partition number that carries any of those ID_FS_*
// fields. That covers Unraid-style layouts (filesystem on partition 1)
// and Windows GPT disks (MSR on 1, NTFS on 2). Every read is a udev
// database file, never blkid, so a standby disk is not opened (Q13).
func (l *Lister) discoveryFS(name string) (fsType, label, uuid string) {
	fsType, label, uuid = l.udevFS(name)
	if fsType != "" || label != "" || uuid != "" {
		return fsType, label, uuid
	}
	for _, part := range l.partitions(name) {
		fsType, label, uuid = l.udevFS(part)
		if fsType != "" || label != "" || uuid != "" {
			return fsType, label, uuid
		}
	}
	return "", "", ""
}

// partitions returns the sysfs names of every partition of diskName,
// ordered by ascending partition number. Names follow the kernel's
// convention: sdX1 / nvme0n1p1 (and the mmcblkN equivalent).
func (l *Lister) partitions(diskName string) []string {
	entries, err := os.ReadDir(l.SysBlockDir)
	if err != nil {
		return nil
	}
	type part struct {
		name string
		num  int
	}
	var parts []part
	for _, e := range entries {
		name := e.Name()
		if !isPartitionOf(diskName, name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(l.SysBlockDir, name, "partition"))
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			continue
		}
		parts = append(parts, part{name: name, num: n})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].num < parts[j].num })
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.name
	}
	return out
}

// isPartitionOf reports whether name is a kernel partition node of disk
// (sda1 of sda, nvme0n1p2 of nvme0n1). The suffix must be all digits, or
// 'p' followed by all digits — never a longer sibling disk name.
func isPartitionOf(disk, name string) bool {
	if !strings.HasPrefix(name, disk) || len(name) <= len(disk) {
		return false
	}
	rest := name[len(disk):]
	if rest[0] == 'p' {
		rest = rest[1:]
		if rest == "" {
			return false
		}
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (l *Lister) udevFS(name string) (fsType, label, uuid string) {
	majmin := readSysString(filepath.Join(l.SysBlockDir, name, "dev"))
	if majmin == "" {
		return "", "", ""
	}
	path := filepath.Join(l.udevDataDir(), "b"+majmin)
	f, err := os.Open(path)
	if err != nil {
		return "", "", ""
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "E:") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "E:"), "=")
		if !ok {
			continue
		}
		switch key {
		case "ID_FS_TYPE":
			fsType = val
		case "ID_FS_LABEL":
			label = val
		case "ID_FS_UUID":
			uuid = val
		}
	}
	return fsType, label, uuid
}
