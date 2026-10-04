package disk

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// unraidPartitionStart is the sector, in 512-byte units, at which Unraid puts
// partition 1 of an array or pool disk, on an MBR and on a GPT disk alike (doc
// 05 §1.1, calibrated on a real 7.3.2 server in doc 08 §2).
const unraidPartitionStart = 64

// LooksLikeUnraidLayout reports whether a disk is laid out the way Unraid lays
// out an array or pool disk: an MBR or GPT partition table whose partition 1
// starts at sector 64 and holds XFS, btrfs or ext4. Unraid sets no filesystem
// label, so the layout is the only sign there is. It is a hint for a warning,
// never a role: a parity disk carries the same layout, and the migration takes
// every role from the capture's disks.ini.
func LooksLikeUnraidLayout(tableType string, part1Start int64, part1FS string) bool {
	switch tableType {
	case "dos", "gpt":
	default:
		return false
	}
	if part1Start != unraidPartitionStart {
		return false
	}
	switch part1FS {
	case "xfs", "btrfs", "ext4":
		return true
	}
	return false
}

// Unraid 7.3's internal boot device (doc 08 §2, from mkbootable in
// unraid/webgui): GPT partitions 1 to 4 carry these names and types. Partition
// 4 may have any name.
const (
	gptTypeBIOSBoot  = "21686148-6449-6e6f-744e-656564454649"
	gptTypeEFI       = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	gptTypeLinuxData = "0fc63daf-8483-4772-8e79-3d69d8477de4"
)

// PartitionEntry is one GPT partition as udev's cached database reports it:
// its number, its name (ID_PART_ENTRY_NAME, already decoded) and its type GUID
// (ID_PART_ENTRY_TYPE).
type PartitionEntry struct {
	Number int
	Name   string
	Type   string
}

// IsUnraidInternalBoot reports whether parts are the partitions of an Unraid
// 7.3 internal boot device: partition 1 a BIOS Boot Partition, 2 an EFI System
// Partition, 3 an Unraid Boot Partition (the ZFS pool the flash lives on) and 4
// a Linux data partition, with the names and type GUIDs mkbootable writes. It
// is Unraid's own detector. A device matching only part of the layout is not
// one, so a ZFS partition on it stays an ordinary ZFS disk.
func IsUnraidInternalBoot(parts []PartitionEntry) bool {
	want := map[int]struct{ name, typ string }{
		1: {"BIOS Boot Partition", gptTypeBIOSBoot},
		2: {"EFI System Partition", gptTypeEFI},
		3: {"Unraid Boot Partition", gptTypeLinuxData},
		4: {"", gptTypeLinuxData},
	}
	found := 0
	for _, p := range parts {
		w, ok := want[p.Number]
		if !ok {
			continue
		}
		if !strings.EqualFold(p.Type, w.typ) || (w.name != "" && p.Name != w.name) {
			return false
		}
		found++
	}
	return found == len(want)
}

func (l *Lister) udevDataDir() string {
	if l.UdevDataDir != "" {
		return l.UdevDataDir
	}
	return "/run/udev/data"
}

// discoveryFS returns udev-cached ID_FS_TYPE, ID_FS_LABEL and ID_FS_UUID
// for the whole-disk sysfs name, falling back to its partitions when the
// whole disk has none, and the sysfs name of the node that carried them.
// Primary filesystem rule: the first partition by ascending sysfs partition
// number that carries any of those ID_FS_* fields. That covers Unraid-style
// layouts (filesystem on partition 1) and Windows GPT disks (MSR on 1, NTFS
// on 2). Every read is a udev database file, never blkid, so a standby disk
// is not opened (Q13).
func (l *Lister) discoveryFS(name string) (node, fsType, label, uuid string) {
	fsType, label, uuid = l.udevFS(name)
	if fsType != "" || label != "" || uuid != "" {
		return name, fsType, label, uuid
	}
	for _, part := range l.partitions(name) {
		fsType, label, uuid = l.udevFS(part)
		if fsType != "" || label != "" || uuid != "" {
			return part, fsType, label, uuid
		}
	}
	return "", "", "", ""
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
	props, _ := l.udevProps(name)
	return props["ID_FS_TYPE"], props["ID_FS_LABEL"], props["ID_FS_UUID"]
}

// udevProps returns every E: property in udev's cached database entry for
// the block device name, and false when there is no entry to read — the
// device has no dev number, or udev has not recorded it.
func (l *Lister) udevProps(name string) (map[string]string, bool) {
	majmin := readSysString(filepath.Join(l.SysBlockDir, name, "dev"))
	if majmin == "" {
		return nil, false
	}
	f, err := os.Open(filepath.Join(l.udevDataDir(), "b"+majmin))
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()

	props := make(map[string]string)
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
		props[key] = val
	}
	return props, true
}

// layoutFacts reads what the Unraid layout heuristic and the internal-boot
// detector need from sysfs and udev's cached database: the partition table
// type, partition 1's start sector and filesystem, and every partition's GPT
// name and type. Nothing is opened, so a standby disk stays asleep.
func (l *Lister) layoutFacts(name string) (looksLikeUnraid, internalBoot bool) {
	props, _ := l.udevProps(name)
	tableType := props["ID_PART_TABLE_TYPE"]
	var entries []PartitionEntry
	var part1Start int64
	part1FS := ""
	for _, part := range l.partitions(name) {
		num, err := readSysInt64(filepath.Join(l.SysBlockDir, part, "partition"))
		if err != nil {
			continue
		}
		pp, _ := l.udevProps(part)
		entries = append(entries, PartitionEntry{Number: int(num), Name: unescape(pp["ID_PART_ENTRY_NAME"], false, true), Type: pp["ID_PART_ENTRY_TYPE"]})
		if num == 1 {
			if start, err := readSysInt64(filepath.Join(l.SysBlockDir, part, "start")); err == nil {
				part1Start = start
			}
			part1FS = pp["ID_FS_TYPE"]
		}
	}
	return LooksLikeUnraidLayout(tableType, part1Start, part1FS), tableType == "gpt" && IsUnraidInternalBoot(entries)
}
