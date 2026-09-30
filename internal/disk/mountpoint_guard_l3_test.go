//go:build l3

// This file runs only inside the L3 VM harness (doc 06 §4), never on the
// host or in the loop-device lab: the lab container has no
// CAP_LINUX_IMMUTABLE, so chattr +i is refused there (Q45 keeps the lab's
// capability set at loop devices and FUSE). scripts/vm/immutable-mountpoint-check.sh
// builds it with `go test -tags l3 -c` on the host (compiling touches no
// device), copies the binary into the guest and runs it there with sudo.
//
// It exercises the production guard — disk.GuardedMounter over
// disk.DirectMounter, both on the real disk.CommandRunner — against a real
// kernel: a real chattr +i on an empty mountpoint, a real ext4 filesystem
// mounting onto that immutable directory, and real writes into the
// unmounted directory.

package disk_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

const l3ImmutableMountRoot = "/mnt/hoserval3immut"

func l3ImmRequire(t *testing.T) string {
	t.Helper()
	if os.Getenv("HOSERVA_LAB_ID") == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own L3 VM, driven by scripts/vm/immutable-mountpoint-check.sh")
	}
	if os.Geteuid() != 0 {
		t.Skip("not running as root — chattr +i and mount need CAP_LINUX_IMMUTABLE and CAP_SYS_ADMIN, and must run via sudo inside the guest")
	}
	root := os.Getenv("HOSERVA_L3_MOUNT_ROOT")
	if root == "" {
		root = l3ImmutableMountRoot
	}
	return root
}

func l3ImmRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

func l3ImmIsMountpoint(path string) bool {
	return exec.Command("mountpoint", "-q", path).Run() == nil
}

func l3ImmHasImmutableFlag(t *testing.T, path string) bool {
	t.Helper()
	out := l3ImmRun(t, "lsattr", "-d", path)
	fields := strings.Fields(out)
	if len(fields) < 2 {
		t.Fatalf("lsattr -d %s: unexpected output %q", path, out)
	}
	return strings.Contains(fields[0], "i")
}

func l3ImmAssertWriteRefused(t *testing.T, dir string) {
	t.Helper()
	err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("landed on the boot device"), 0o644)
	if err == nil {
		t.Fatalf("writing a file into the unmounted %s succeeded: the write landed on the boot device", dir)
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("writing a file into the unmounted %s failed with %v, want EPERM", dir, err)
	}
	err = os.Mkdir(filepath.Join(dir, "stray-dir"), 0o755)
	if err == nil {
		t.Fatalf("creating a directory in the unmounted %s succeeded: the write landed on the boot device", dir)
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("creating a directory in the unmounted %s failed with %v, want EPERM", dir, err)
	}
}

func l3ImmAssertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("unmounted %s holds %v, want it empty — something landed on the boot device", dir, names)
	}
}

func l3ImmRandomUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generating a UUID: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// TestL3ImmutableMountpoint_GuardsUnmountedSlot covers Q69's three claims on
// a real kernel: the guard sets the flag on an empty mountpoint, a real disk
// filesystem still mounts onto the immutable directory (and its root is not
// immutable), and a write into the unmounted directory returns EPERM instead
// of landing on the boot device.
func TestL3ImmutableMountpoint_GuardsUnmountedSlot(t *testing.T) {
	root := l3ImmRequire(t)
	ctx := context.Background()
	runner := disk.CommandRunner{}

	slot := filepath.Join(root, "disk1")
	t.Cleanup(func() {
		if l3ImmIsMountpoint(slot) {
			if out, err := exec.Command("umount", slot).CombinedOutput(); err != nil {
				t.Logf("cleanup: umount %s: %v: %s", slot, err, strings.TrimSpace(string(out)))
				return
			}
		}
		if out, err := exec.Command("chattr", "-i", slot).CombinedOutput(); err != nil {
			t.Logf("cleanup: chattr -i %s: %v: %s", slot, err, strings.TrimSpace(string(out)))
		}
		if err := os.RemoveAll(root); err != nil {
			t.Logf("cleanup: removing %s: %v", root, err)
		}
	})

	img := filepath.Join(t.TempDir(), "disk1.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatalf("creating disk image: %v", err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatalf("sizing disk image: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing disk image: %v", err)
	}
	l3ImmRun(t, "mkfs.ext4", "-F", "-q", img)
	loopDev := l3ImmRun(t, "losetup", "--find", "--show", img)
	t.Cleanup(func() {
		if out, err := exec.Command("losetup", "-d", loopDev).CombinedOutput(); err != nil {
			t.Logf("cleanup: losetup -d %s: %v: %s", loopDev, err, strings.TrimSpace(string(out)))
		}
	})
	uuid := l3ImmRun(t, "blkid", "-s", "UUID", "-o", "value", loopDev)
	if uuid == "" {
		t.Fatalf("blkid reported no UUID for %s", loopDev)
	}

	mounter := disk.GuardedMounter{Mounter: disk.DirectMounter{Runner: runner}, Runner: runner}

	t.Run("a disk that does not mount leaves the slot immutable and empty", func(t *testing.T) {
		missing := disk.MountUnit{Where: slot, UUID: l3ImmRandomUUID(t), Filesystem: disk.EXT4}
		// DirectMounter mounts with nofail, so mount(8) reports success for
		// a UUID no device carries; only the resulting mount state matters.
		if err := mounter.Mount(ctx, missing); err != nil {
			t.Logf("Mount of the absent disk returned: %v", err)
		}
		if l3ImmIsMountpoint(slot) {
			t.Fatalf("%s is a mountpoint although no disk with UUID %s exists", slot, missing.UUID)
		}
		if !l3ImmHasImmutableFlag(t, slot) {
			t.Fatalf("lsattr -d %s shows no immutable flag after GuardedMounter.Mount ran", slot)
		}
		l3ImmAssertWriteRefused(t, slot)
		l3ImmAssertEmpty(t, slot)
	})

	unit := disk.MountUnit{Where: slot, UUID: uuid, Filesystem: disk.EXT4}

	t.Run("a real disk mounts onto the immutable directory and its root is writable", func(t *testing.T) {
		if err := mounter.Mount(ctx, unit); err != nil {
			t.Fatalf("mounting %s (UUID %s) onto the immutable %s: %v", loopDev, uuid, slot, err)
		}
		if !l3ImmIsMountpoint(slot) {
			t.Fatalf("%s is not a mountpoint after Mount returned", slot)
		}
		if got := l3ImmRun(t, "findmnt", "-n", "-o", "UUID", slot); got != uuid {
			t.Fatalf("findmnt reports UUID %q at %s, want %q", got, slot, uuid)
		}
		if l3ImmHasImmutableFlag(t, slot) {
			t.Fatalf("the mounted filesystem's root %s carries the immutable flag", slot)
		}
		if err := os.WriteFile(filepath.Join(slot, "on-disk.txt"), []byte("on the data disk"), 0o644); err != nil {
			t.Fatalf("writing to the mounted disk: %v", err)
		}
	})

	t.Run("unmounting exposes the immutable directory again", func(t *testing.T) {
		if err := mounter.Unmount(ctx, unit); err != nil {
			t.Fatalf("unmounting %s: %v", slot, err)
		}
		if l3ImmIsMountpoint(slot) {
			t.Fatalf("%s is still a mountpoint after Unmount returned", slot)
		}
		if !l3ImmHasImmutableFlag(t, slot) {
			t.Fatalf("lsattr -d %s shows no immutable flag after the disk was unmounted", slot)
		}
		l3ImmAssertWriteRefused(t, slot)
		l3ImmAssertEmpty(t, slot)
	})

	t.Run("the data written while mounted is on the disk, not the boot device", func(t *testing.T) {
		if err := mounter.Mount(ctx, unit); err != nil {
			t.Fatalf("remounting %s onto %s: %v", loopDev, slot, err)
		}
		got, err := os.ReadFile(filepath.Join(slot, "on-disk.txt"))
		if err != nil {
			t.Fatalf("reading the file written while mounted: %v", err)
		}
		if string(got) != "on the data disk" {
			t.Fatalf("file content after remount = %q, want %q", got, "on the data disk")
		}
	})
}
