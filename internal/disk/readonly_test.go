package disk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mountTableRunner stands in for mount(8), findmnt(8) and umount(8) over a
// mountinfo file the test controls: mount writes the entry mountLine gives (or
// nothing), umount removes it (unless umountFails), blkid answers uuid (or
// fails with probeErr). findmnt is not scripted: a mount judged through it
// would be an unexpected command.
type mountTableRunner struct {
	table       string
	where       string
	mountLine   string
	mountErr    error
	umountFails bool
	uuid        string
	probeErr    error
	calls       [][]string
}

func (r *mountTableRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	switch name {
	case "mount":
		if r.mountLine != "" {
			if err := os.WriteFile(r.table, []byte(r.mountLine+"\n"), 0o644); err != nil {
				return nil, err
			}
		}
		return nil, r.mountErr
	case "umount":
		if r.umountFails {
			return nil, errors.New("target is busy")
		}
		return nil, os.WriteFile(r.table, nil, 0o644)
	case "blkid":
		if r.probeErr != nil {
			return nil, r.probeErr
		}
		return []byte(r.uuid + "\n"), nil
	}
	return nil, fmt.Errorf("unexpected command %s", name)
}

func (r *mountTableRunner) ran(name string) bool {
	for _, c := range r.calls {
		if c[0] == name {
			return true
		}
	}
	return false
}

func mountLineFor(where, mountOpts, fsType, superOpts string) string {
	return fmt.Sprintf("40 25 8:17 / %s %s - %s /dev/sdb1 %s", where, mountOpts, fsType, superOpts)
}

const roDevice = "/dev/sdb1"

func newROFixture(t *testing.T) (*mountTableRunner, KernelReadOnlyMounter, string) {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	where := filepath.Join(resolved, "stick")
	table := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(table, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &mountTableRunner{table: table, where: where, uuid: "ABCD-1234"}
	return r, KernelReadOnlyMounter{Runner: r, MountInfo: table}, where
}

func TestKernelReadOnlyMounter_MountsReadOnlyAsAnArgv(t *testing.T) {
	r, m, where := newROFixture(t)
	r.mountLine = mountLineFor(where, "ro,nosuid,nodev,noexec,noatime", "vfat", "ro,fmask=0022")

	if err := m.MountReadOnly(context.Background(), "vfat", roDevice, "ABCD-1234", where); err != nil {
		t.Fatalf("MountReadOnly: %v", err)
	}
	var mount []string
	for _, c := range r.calls {
		if c[0] == "mount" {
			mount = c
		}
	}
	want := []string{"mount", "-t", "vfat", "-o", "ro,noatime,nodiratime,nosuid,nodev,noexec,utf8=1", roDevice, where}
	if strings.Join(mount, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("mount argv = %q, want %q", mount, want)
	}
	var probe []string
	for _, c := range r.calls {
		if c[0] == "blkid" {
			probe = c
		}
	}
	wantProbe := []string{"blkid", "-p", "-s", "UUID", "-o", "value", roDevice}
	if strings.Join(probe, "\x00") != strings.Join(wantProbe, "\x00") {
		t.Fatalf("probe argv = %q, want %q", probe, wantProbe)
	}
	if r.ran("findmnt") {
		t.Fatal("the UUID was read through findmnt, which resolves the source through udev")
	}
	if info, err := os.Stat(where); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("mountpoint = %v, %v; want a 0700 directory", info, err)
	}
}

// A source data disk's journal is never replayed by the mount that reads it: a
// plain ro XFS mount still writes the superblock and the log (doc 08 §2), and a
// plain ro btrfs mount replays a pending tree log, so each filesystem is
// mounted with the option that stops it.
func TestKernelReadOnlyMounter_SourceDiskOptionsNeverReplayAJournal(t *testing.T) {
	for fsType, want := range map[string]string{
		"xfs":   "ro,norecovery,noatime,nodiratime,nosuid,nodev,noexec",
		"ext4":  "ro,noload,noatime,nodiratime,nosuid,nodev,noexec",
		"btrfs": "ro,rescue=nologreplay,noatime,nodiratime,nosuid,nodev,noexec",
	} {
		t.Run(fsType, func(t *testing.T) {
			r, m, where := newROFixture(t)
			r.uuid = "11111111-2222-3333-4444-555555555555"
			r.mountLine = mountLineFor(where, "ro,nosuid", fsType, "ro")
			if err := m.MountReadOnly(context.Background(), fsType, roDevice, r.uuid, where); err != nil {
				t.Fatalf("MountReadOnly: %v", err)
			}
			var mount []string
			for _, c := range r.calls {
				if c[0] == "mount" {
					mount = c
				}
			}
			wantArgv := []string{"mount", "-t", fsType, "-o", want, roDevice, where}
			if strings.Join(mount, "\x00") != strings.Join(wantArgv, "\x00") {
				t.Fatalf("mount argv = %q, want %q", mount, wantArgv)
			}
		})
	}
}

// A mount that came up read-write is the one outcome that costs the user their
// rollback, so each way the table can fail to show read-only is refused and the
// mount is undone.
func TestKernelReadOnlyMounter_RefusesAndUndoesAnythingNotShownReadOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		mountOpts, fsType, superOpts, uuid, want string
		inTable                                  bool
	}{
		"mounted read-write":        {"rw,nosuid,nodev,noexec", "vfat", "ro", "ABCD-1234", "without ro", true},
		"superblock read-write":     {"ro,nosuid", "vfat", "rw", "ABCD-1234", "superblock is not read-only", true},
		"another filesystem's UUID": {"ro", "vfat", "ro", "FFFF-0000", "holds filesystem UUID FFFF-0000, want ABCD-1234", true},
		"an ro substring is not ro": {"rw,errors=ro", "vfat", "ro", "ABCD-1234", "without ro", true},
		"another filesystem type":   {"ro", "ext4", "ro", "ABCD-1234", `mounted as "ext4"`, true},
		"not in the table at all":   {"", "", "", "ABCD-1234", "not in the mount table", false},
	} {
		t.Run(name, func(t *testing.T) {
			r, m, where := newROFixture(t)
			r.uuid = tc.uuid
			if tc.inTable {
				r.mountLine = mountLineFor(where, tc.mountOpts, tc.fsType, tc.superOpts)
			}
			err := m.MountReadOnly(context.Background(), "vfat", roDevice, "ABCD-1234", where)
			if !errors.Is(err, ErrReadOnlyMount) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("MountReadOnly = %v, want %v containing %q", err, ErrReadOnlyMount, tc.want)
			}
			if tc.inTable && !r.ran("umount") {
				t.Fatal("a mount that was not shown read-only was left mounted")
			}
			if mounted, err := m.IsMounted(context.Background(), where); err != nil || mounted {
				t.Fatalf("IsMounted after the refusal = %v, %v; want unmounted", mounted, err)
			}
		})
	}
}

// The device probe is the only evidence of which filesystem was mounted, so one
// that fails is a refusal and the mount is undone, never "fine".
func TestKernelReadOnlyMounter_UndoesAMountWhoseDeviceCannotBeProbed(t *testing.T) {
	r, m, where := newROFixture(t)
	r.mountLine = mountLineFor(where, "ro", "vfat", "ro")
	r.probeErr = errors.New("exit status 2")
	err := m.MountReadOnly(context.Background(), "vfat", roDevice, "ABCD-1234", where)
	if !errors.Is(err, ErrReadOnlyMount) || !strings.Contains(err.Error(), "probing the filesystem UUID") {
		t.Fatalf("MountReadOnly = %v, want %v from the probe", err, ErrReadOnlyMount)
	}
	if !r.ran("umount") {
		t.Fatal("a mount whose filesystem could not be identified was left mounted")
	}
	if mounted, err := m.IsMounted(context.Background(), where); err != nil || mounted {
		t.Fatalf("IsMounted after the refusal = %v, %v; want unmounted", mounted, err)
	}
}

func TestKernelReadOnlyMounter_ReportsAMountItCouldNotUndo(t *testing.T) {
	r, m, where := newROFixture(t)
	r.mountLine = mountLineFor(where, "rw", "vfat", "rw")
	r.umountFails = true
	err := m.MountReadOnly(context.Background(), "vfat", roDevice, "ABCD-1234", where)
	if err == nil || !strings.Contains(err.Error(), "without ro") || !strings.Contains(err.Error(), "still mounted") {
		t.Fatalf("MountReadOnly = %v, want both the refusal and that the mount is still there", err)
	}
}

func TestKernelReadOnlyMounter_RefusesBeforeRunningMount(t *testing.T) {
	for name, tc := range map[string]struct {
		fsType, device, uuid string
		mountedAlready       bool
	}{
		"a filesystem type with no known read-only options": {"ntfs", roDevice, "ABCD-1234", false},
		"a UUID that is an option":                          {"vfat", roDevice, "-oremount,rw", false},
		"an empty UUID":                                     {"vfat", roDevice, "", false},
		"a path that is already a mountpoint":               {"vfat", roDevice, "ABCD-1234", true},
		"a device that is an option":                        {"vfat", "-oremount,rw", "ABCD-1234", false},
		"a device that is not under /dev":                   {"vfat", "stick.img", "ABCD-1234", false},
		"an empty device":                                   {"vfat", "", "ABCD-1234", false},
	} {
		t.Run(name, func(t *testing.T) {
			r, m, where := newROFixture(t)
			if tc.mountedAlready {
				if err := os.WriteFile(r.table, []byte(mountLineFor(where, "rw", "xfs", "rw")+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := m.MountReadOnly(context.Background(), tc.fsType, tc.device, tc.uuid, where)
			if !errors.Is(err, ErrReadOnlyMount) {
				t.Fatalf("MountReadOnly = %v, want %v", err, ErrReadOnlyMount)
			}
			if r.ran("mount") || r.ran("umount") {
				t.Fatalf("ran %v for a refused mount", r.calls)
			}
		})
	}
}

func TestKernelReadOnlyMounter_Unmount(t *testing.T) {
	t.Run("a path that is not mounted runs nothing", func(t *testing.T) {
		r, m, where := newROFixture(t)
		if err := m.Unmount(context.Background(), where); err != nil || len(r.calls) != 0 {
			t.Fatalf("Unmount = %v after %v, want nil and no command", err, r.calls)
		}
	})
	t.Run("a mount is released", func(t *testing.T) {
		r, m, where := newROFixture(t)
		if err := os.WriteFile(r.table, []byte(mountLineFor(where, "ro", "vfat", "ro")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := m.Unmount(context.Background(), where); err != nil {
			t.Fatal(err)
		}
		if mounted, _ := m.IsMounted(context.Background(), where); mounted {
			t.Fatal("still mounted")
		}
	})
	t.Run("exit 0 while the table still lists it is a failure", func(t *testing.T) {
		_, m, where := newROFixture(t)
		stuck := KernelReadOnlyMounter{Runner: noopUmountRunner{}, MountInfo: m.MountInfo}
		if err := os.WriteFile(m.MountInfo, []byte(mountLineFor(where, "ro", "vfat", "ro")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := stuck.Unmount(context.Background(), where); err == nil || !strings.Contains(err.Error(), "still mounted") {
			t.Fatalf("Unmount = %v, want still mounted", err)
		}
	})
	t.Run("a failing umount is reported", func(t *testing.T) {
		r, m, where := newROFixture(t)
		r.umountFails = true
		if err := os.WriteFile(r.table, []byte(mountLineFor(where, "ro", "vfat", "ro")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := m.Unmount(context.Background(), where); err == nil || !strings.Contains(err.Error(), "busy") {
			t.Fatalf("Unmount = %v, want the busy error", err)
		}
	})
}

type noopUmountRunner struct{}

func (noopUmountRunner) Run(context.Context, string, ...string) ([]byte, error) { return nil, nil }

func TestMountEntry_TakesTheLastEntryAtAPathAndDecodesEscapes(t *testing.T) {
	table := strings.Join([]string{
		`36 25 8:1 / /mnt/a\040b rw,relatime shared:1 - xfs /dev/sda1 rw`,
		`41 25 8:17 / /mnt/a\040b rw - vfat /dev/sdb1 rw`,
		`42 25 8:33 / /mnt/a\040b ro,nosuid master:2 master:3 - vfat /dev/sdc1 ro,fmask=0022`,
		`43 25 8:49 / /mnt/other rw - ext4 /dev/sdd1 rw`,
	}, "\n")
	m, found, err := mountEntry(strings.NewReader(table), "/mnt/a b")
	if err != nil || !found {
		t.Fatalf("mountEntry = %v, %v", found, err)
	}
	if m.mountOptions != "ro,nosuid" || m.fsType != "vfat" || m.superOptions != "ro,fmask=0022" {
		t.Fatalf("entry = %+v, want the last entry's", m)
	}
	if _, found, _ := mountEntry(strings.NewReader(table), "/mnt/a"); found {
		t.Fatal("a prefix of a mountpoint matched")
	}
}

func TestFakeReadOnlyMounter_RecordsAndRefusesAStackedMount(t *testing.T) {
	f := NewFakeReadOnlyMounter()
	where := filepath.Join(t.TempDir(), "stick")
	if err := f.MountReadOnly(context.Background(), "vfat", roDevice, "ABCD-1234", where); err != nil {
		t.Fatal(err)
	}
	if err := f.MountReadOnly(context.Background(), "vfat", roDevice, "ABCD-1234", where); !errors.Is(err, ErrReadOnlyMount) {
		t.Fatalf("second mount = %v, want %v", err, ErrReadOnlyMount)
	}
	if got := f.MountedPaths(); len(got) != 1 || got[0] != where {
		t.Fatalf("mounted = %v", got)
	}
	if err := f.Unmount(context.Background(), where); err != nil || len(f.MountedPaths()) != 0 {
		t.Fatalf("Unmount = %v, mounted = %v", err, f.MountedPaths())
	}
}
