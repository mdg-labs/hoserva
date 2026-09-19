package disk

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
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

// discoveryFS returns udev-cached ID_FS_TYPE and ID_FS_LABEL for the
// whole-disk sysfs name, falling back to its first partition when the
// whole disk has none — Unraid (and most NAS layouts) put the filesystem
// on partition 1. Both reads are udev database files, never blkid, so a
// standby disk is not opened.
func (l *Lister) discoveryFS(name string) (fsType, label string) {
	fsType, label = l.udevFS(name)
	if fsType != "" || label != "" {
		return fsType, label
	}
	part := l.firstPartition(name)
	if part == "" {
		return "", ""
	}
	return l.udevFS(part)
}

func (l *Lister) firstPartition(diskName string) string {
	for _, cand := range []string{diskName + "1", diskName + "p1"} {
		if _, err := os.Stat(filepath.Join(l.SysBlockDir, cand, "partition")); err == nil {
			return cand
		}
	}
	return ""
}

func (l *Lister) udevFS(name string) (fsType, label string) {
	majmin := readSysString(filepath.Join(l.SysBlockDir, name, "dev"))
	if majmin == "" {
		return "", ""
	}
	path := filepath.Join(l.udevDataDir(), "b"+majmin)
	f, err := os.Open(path)
	if err != nil {
		return "", ""
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
		}
	}
	return fsType, label
}
