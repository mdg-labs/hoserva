package disk

import (
	"bufio"
	"os"
	"regexp"
	"strings"
)

// MountEntry is one parsed line of a /proc/mounts-style listing: device,
// mountpoint and filesystem type.
type MountEntry struct {
	Device     string
	MountPoint string
	FSType     string
}

// ReadProcMounts parses a mounts file at path (normally /proc/mounts). It
// only reads kernel state the system already publishes, so it never
// touches a block device directly.
func ReadProcMounts(path string) ([]MountEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []MountEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		out = append(out, MountEntry{Device: fields[0], MountPoint: fields[1], FSType: fields[2]})
	}
	return out, scanner.Err()
}

// BootDevice resolves the whole-disk device backing the root filesystem
// from a mounts listing, so it can always be identified and excluded from
// anything destructive (doc 02 §4). It reports false if no entry mounts
// "/" on a real block device.
func BootDevice(mounts []MountEntry) (string, bool) {
	for _, m := range mounts {
		if m.MountPoint == "/" && strings.HasPrefix(m.Device, "/dev/") {
			return WholeDiskDevice(m.Device), true
		}
	}
	return "", false
}

var (
	nvmeStylePartition   = regexp.MustCompile(`^(.+[0-9])p([0-9]+)$`)
	letterStylePartition = regexp.MustCompile(`^(.+[a-zA-Z])([0-9]+)$`)
)

// WholeDiskDevice strips a trailing partition number from a partition
// device path, returning the whole disk it belongs to. NVMe- and
// mmcblk-style names separate the partition number with a "p"
// ("/dev/nvme0n1p2" -> "/dev/nvme0n1", where "nvme0n1" is itself the
// whole disk's own name and must not be stripped further); SCSI/ATA/
// virtio-style names do not ("/dev/sda2" -> "/dev/sda").
func WholeDiskDevice(dev string) string {
	idx := strings.LastIndex(dev, "/")
	dir, base := dev[:idx+1], dev[idx+1:]

	if strings.HasPrefix(base, "nvme") || strings.HasPrefix(base, "mmcblk") || strings.HasPrefix(base, "loop") {
		if m := nvmeStylePartition.FindStringSubmatch(base); m != nil {
			return dir + m[1]
		}
		return dev
	}
	if m := letterStylePartition.FindStringSubmatch(base); m != nil {
		return dir + m[1]
	}
	return dev
}
