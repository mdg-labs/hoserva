package disk

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// BootDevices resolves every physical whole-disk device backing the root
// filesystem from a mounts listing, so every one of them can always be
// identified and excluded from anything destructive (doc 02 §4). A root
// mount is frequently not a physical disk's own partition directly: it
// can be a device-mapper name (LVM, LUKS), an MD software-RAID array, or
// the kernel's own "/dev/root" alias — each of those is a virtual device
// that can itself be built from more than one physical disk, so this
// walks sysBlockDir's "slaves" links recursively until it bottoms out at
// real, physical leaf devices rather than returning the one virtual path.
// Classification must fail closed: any device it cannot resolve is
// reported as an error, never silently dropped, since a boot disk this
// misses would look like an ordinary, formattable disk to every
// destructive caller downstream.
//
// It returns (nil, nil) if no entry mounts "/" on a device at all — a
// mounts listing this narrow is a caller/environment issue for the code
// that built it to decide on, not something this function can resolve.
func BootDevices(mounts []MountEntry, sysBlockDir string) ([]string, error) {
	for _, m := range mounts {
		if m.MountPoint != "/" {
			continue
		}
		if !strings.HasPrefix(m.Device, "/dev/") {
			return nil, nil
		}
		return resolvePhysicalDevices(m.Device, sysBlockDir)
	}
	return nil, nil
}

// resolvePhysicalDevices resolves rawDevice (e.g. "/dev/sda2",
// "/dev/mapper/vg-root", "/dev/md0", "/dev/root") to the sorted, deduped
// set of physical whole-disk device paths that ultimately back it.
func resolvePhysicalDevices(rawDevice, sysBlockDir string) ([]string, error) {
	name, err := blockDeviceName(rawDevice, sysBlockDir)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var physical []string
	var walk func(name string) error
	walk = func(name string) error {
		slaves, err := os.ReadDir(filepath.Join(sysBlockDir, name, "slaves"))
		if err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("disk: reading slaves of %s: %w", name, err)
			}
			// No "slaves" directory: name is itself a physical leaf
			// device (or one of its partitions).
			whole := WholeDiskDevice("/dev/" + name)
			if !seen[whole] {
				seen[whole] = true
				physical = append(physical, whole)
			}
			return nil
		}
		for _, s := range slaves {
			if err := walk(s.Name()); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(name); err != nil {
		return nil, err
	}

	sort.Strings(physical)
	return physical, nil
}

// blockDeviceName resolves rawDevice to the kernel block device name
// (e.g. "sda2", "dm-0", "md0") sysBlockDir's own entries are keyed by.
func blockDeviceName(rawDevice, sysBlockDir string) (string, error) {
	base := filepath.Base(rawDevice)

	if _, err := os.Stat(filepath.Join(sysBlockDir, base)); err == nil {
		return base, nil
	}

	// Not a kernel device name directly — try it as a device-mapper
	// friendly name (/dev/mapper/<name>), resolved via each dm-N's own
	// sysfs "dm/name" file, so this needs nothing outside sysBlockDir
	// (never a /dev/mapper symlink read) and stays fixture-testable.
	entries, err := os.ReadDir(sysBlockDir)
	if err != nil {
		return "", fmt.Errorf("disk: reading %s: %w", sysBlockDir, err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "dm-") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(sysBlockDir, e.Name(), "dm", "name"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(content)) == base {
			return e.Name(), nil
		}
	}

	return "", fmt.Errorf("disk: cannot resolve %s to a kernel block device under %s", rawDevice, sysBlockDir)
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
