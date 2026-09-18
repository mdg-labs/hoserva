package disk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Lister enumerates the block devices Hoserva can manage (doc 02 §4):
// every whole-disk entry under SysBlockDir, identified via ByIDDir (Q21)
// and marked when it backs the mountpoint ProcMounts resolves as "/" —
// the boot device, always identified and always excluded from anything
// destructive. List reads only sysfs, /dev/disk/by-id and a mounts
// listing; it never queries a device's SMART or power state, so it never
// risks waking a disk (doc 02 §1, §4).
type Lister struct {
	SysBlockDir string
	ByIDDir     string
	ProcMounts  string
}

// NewLister returns a Lister reading the real system paths.
func NewLister() *Lister {
	return &Lister{
		SysBlockDir: "/sys/class/block",
		ByIDDir:     "/dev/disk/by-id",
		ProcMounts:  "/proc/mounts",
	}
}

// blockDeviceSkipPrefixes names /sys/class/block entries that are never
// disks Hoserva manages: loopback, device-mapper, optical, RAM and
// software-RAID devices.
var blockDeviceSkipPrefixes = []string{"loop", "dm-", "sr", "ram", "zram", "md"}

// List enumerates every whole-disk block device.
func (l *Lister) List(ctx context.Context) ([]Disk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(l.SysBlockDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", l.SysBlockDir, err)
	}

	byID, err := scanByID(l.ByIDDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", l.ByIDDir, err)
	}

	// Boot-disk classification fails closed: a mounts listing List can't
	// read, or a root device BootDevices can't resolve to real physical
	// disks, must stop List outright rather than silently continue with
	// no boot device known — otherwise every disk below would come back
	// with Boot: false, and every destructive caller downstream would
	// treat the real boot disk as an ordinary, formattable one.
	mounts, err := ReadProcMounts(l.ProcMounts)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", l.ProcMounts, err)
	}
	bootDevs, err := BootDevices(mounts, l.SysBlockDir)
	if err != nil {
		return nil, fmt.Errorf("resolving the boot device: %w", err)
	}
	bootSet := make(map[string]bool, len(bootDevs))
	for _, d := range bootDevs {
		bootSet[d] = true
	}

	var disks []Disk
	for _, e := range entries {
		name := e.Name()
		if skipBlockDevice(name) {
			continue
		}
		if _, err := os.Stat(filepath.Join(l.SysBlockDir, name, "partition")); err == nil {
			continue // a partition, not a whole disk
		}

		size, err := readSysInt64(filepath.Join(l.SysBlockDir, name, "size"))
		if err != nil {
			continue // unreadable device: skip it rather than fail the whole list
		}

		model := readSysString(filepath.Join(l.SysBlockDir, name, "device", "model"))
		id := ResolveIdentity(byID[name])
		dev := "/dev/" + name

		disks = append(disks, Disk{
			Device:       dev,
			Size:         size * 512, // /sys/class/block/<dev>/size is always in 512-byte sectors
			Model:        model,
			Serial:       id.Serial,
			WWN:          id.WWN,
			WeakIdentity: id.WeakIdentity,
			ByIDName:     id.ByIDName,
			Boot:         bootSet[dev],
		})
	}

	sort.Slice(disks, func(i, j int) bool { return disks[i].Device < disks[j].Device })
	return disks, nil
}

func skipBlockDevice(name string) bool {
	for _, p := range blockDeviceSkipPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func readSysInt64(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
}

func readSysString(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

var partitionByIDSuffix = regexp.MustCompile(`-part\d+$`)

// scanByID reads dir (normally /dev/disk/by-id) and returns, for every
// whole-disk entry, the device name it resolves to mapped to every
// by-id link name found for it. Partition entries (a "-partN" suffix)
// are excluded — they name a partition's identity, not the disk's.
func scanByID(dir string) (map[string][]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	out := make(map[string][]string)
	for _, e := range entries {
		name := e.Name()
		if partitionByIDSuffix.MatchString(name) {
			continue
		}
		target, err := os.Readlink(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		dev := filepath.Base(target)
		out[dev] = append(out[dev], name)
	}
	return out, nil
}
