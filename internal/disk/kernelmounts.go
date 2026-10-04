package disk

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultMountInfo is the kernel mount table of the calling process's own
// mount namespace.
const DefaultMountInfo = "/proc/self/mountinfo"

// KernelMounts reads and changes mounts by path, judged from the kernel
// mount table rather than from any command's exit status (doc 02 §4,
// UR4 and UR5). It does not care what mounted a path: a systemd unit, a
// raw mount or a stacked mount all appear in the table the same way.
type KernelMounts struct {
	Runner Runner
	// MountInfo is the mountinfo file to read; empty uses DefaultMountInfo.
	MountInfo string
}

// IsMounted reports whether path is a mountpoint in the kernel mount
// table. A path that does not exist is never in the table, so it is not
// mounted.
func (k KernelMounts) IsMounted(ctx context.Context, path string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	src := k.MountInfo
	if src == "" {
		src = DefaultMountInfo
	}
	f, err := os.Open(src)
	if err != nil {
		return false, fmt.Errorf("disk: reading the mount table: %w", err)
	}
	defer func() { _ = f.Close() }()
	points, err := ParseMountInfo(f)
	if err != nil {
		return false, err
	}
	want := filepath.Clean(path)
	for _, p := range points {
		if p == want {
			return true, nil
		}
	}
	return false, nil
}

// UnmountOnce runs one `umount <path>`, as an argv. Its exit status is
// only reported; whether path is still mounted is IsMounted's answer.
func (k KernelMounts) UnmountOnce(ctx context.Context, path string) error {
	if _, err := k.Runner.Run(ctx, "umount", path); err != nil {
		return fmt.Errorf("disk: unmounting %s: %w", path, err)
	}
	return nil
}

// Mount mounts unit.UUID at unit.Where by filesystem UUID, creating the
// mountpoint if needed (DirectMounter.Mount, which also confirms the UUID
// is mounted there). The data-disk upgrade job mounts through it.
func (k KernelMounts) Mount(ctx context.Context, unit MountUnit) error {
	return DirectMounter{Runner: k.Runner}.Mount(ctx, unit)
}

// MountedUUID reads the filesystem UUID of the source mounted at path
// from the mount table (MountedUUID).
func (k KernelMounts) MountedUUID(ctx context.Context, path string) (string, error) {
	return MountedUUID(ctx, k.Runner, path)
}

// ConfirmMountedUUID returns nil only when where is mounted with
// filesystem UUID uuid, read from the mount table (doc 02 §4 UR4).
func ConfirmMountedUUID(ctx context.Context, r Runner, where, uuid string) error {
	got, err := MountedUUID(ctx, r, where)
	if err != nil {
		return err
	}
	if got != uuid {
		return fmt.Errorf("disk: %s holds filesystem UUID %s, want %s", where, got, uuid)
	}
	return nil
}

// MountedSource reads the device the filesystem mounted at where came from,
// through findmnt. A btrfs subvolume suffix ("/dev/sdb1[/sub]") is dropped.
func MountedSource(ctx context.Context, r Runner, where string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, err := r.Run(ctx, "findmnt", "-n", "-o", "SOURCE", where)
	if err != nil {
		return "", fmt.Errorf("disk: reading the device mounted at %s: %w", where, err)
	}
	src := strings.TrimSpace(string(out))
	if i := strings.Index(src, "["); i >= 0 {
		src = src[:i]
	}
	if src == "" {
		return "", fmt.Errorf("disk: %s reported no mounted source device", where)
	}
	return src, nil
}

// ConfirmMountedSource returns nil only when the filesystem mounted at where
// came from the device what names (a device node or a /dev/disk/by-id path),
// both resolved to the node they point at. A UUID cannot say this: a byte copy
// of a filesystem on another disk, as a former Unraid parity disk can be,
// carries the same one.
func ConfirmMountedSource(ctx context.Context, r Runner, where, what string) error {
	got, err := MountedSource(ctx, r, where)
	if err != nil {
		return err
	}
	want, err := filepath.EvalSymlinks(what)
	if err != nil {
		return fmt.Errorf("disk: resolving %s: %w", what, err)
	}
	have, err := filepath.EvalSymlinks(got)
	if err != nil {
		return fmt.Errorf("disk: resolving %s: %w", got, err)
	}
	if want != have {
		return fmt.Errorf("disk: %s is mounted from %s, want %s (%s)", where, have, want, what)
	}
	return nil
}

// ConfirmMountedReadOnly returns nil only when the mount at where is
// read-only, read from the mount table through findmnt.
func ConfirmMountedReadOnly(ctx context.Context, r Runner, where string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	out, err := r.Run(ctx, "findmnt", "-n", "-o", "OPTIONS", where)
	if err != nil {
		return fmt.Errorf("disk: reading the mount options of %s: %w", where, err)
	}
	if !hasOption(strings.TrimSpace(string(out)), "ro") {
		return fmt.Errorf("disk: %s is mounted read-write (%s)", where, strings.TrimSpace(string(out)))
	}
	return nil
}

// MountedSource implements the source check of job.ArrayDiskUUIDCheck.
func (k KernelMounts) MountedSource(ctx context.Context, path string) (string, error) {
	return MountedSource(ctx, k.Runner, path)
}

// ParseMountInfo returns every mountpoint listed in a mountinfo(5) table,
// in table order, with the kernel's octal escapes (\040 for a space and
// so on) decoded.
func ParseMountInfo(r io.Reader) ([]string, error) {
	var points []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		points = append(points, filepath.Clean(unescapeMountInfo(fields[4])))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("disk: parsing the mount table: %w", err)
	}
	return points, nil
}

func unescapeMountInfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
