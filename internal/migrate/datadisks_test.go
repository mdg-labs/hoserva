package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// tree is what a fake mount of a data disk shows.
type tree struct {
	files map[string]string
	// big maps a path to a size; the file is sparse.
	big   map[string]int64
	links map[string]string
	fifos []string
	dirs  []string
}

func (tr tree) populate(t *testing.T, root string) {
	t.Helper()
	put := func(rel string) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for rel, content := range tr.files {
		if err := os.WriteFile(put(rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for rel, size := range tr.big {
		if err := os.WriteFile(put(rel), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(filepath.Join(root, filepath.FromSlash(rel)), size); err != nil {
			t.Fatal(err)
		}
	}
	for rel, target := range tr.links {
		if err := os.Symlink(target, put(rel)); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range tr.fifos {
		if err := syscall.Mkfifo(put(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range tr.dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// dataEnv is this machine as a scan of an Unraid fixture sees it: the fixture's
// disks, a scripted runner for the filesystem checks, and a fake read-only
// mounter that shows each disk's tree when it is mounted.
type dataEnv struct {
	t       *testing.T
	dir     string
	spec    fixtureSpec
	files   map[string][]byte
	disks   *disk.FakeProvider
	runner  *disk.FakeRunner
	mounter *disk.FakeReadOnlyMounter
	trees   map[string]tree // by the filesystem node, /dev/sdc1
	dev     map[string]string
	opts    ScanOptions
	scanner *Scanner
}

func newDataEnv(t *testing.T, variant string) *dataEnv {
	t.Helper()
	files, spec := flashTree(t, variant)
	e := &dataEnv{
		t: t, dir: t.TempDir(), spec: spec, files: files,
		disks: disk.NewFakeProvider(), runner: disk.NewFakeRunner(), mounter: disk.NewFakeReadOnlyMounter(),
		trees: map[string]tree{}, dev: map[string]string{},
	}
	for i, d := range spec.disks {
		fsType := d.fs
		if d.kind == "parity" {
			fsType = "xfs"
		}
		dev := fmt.Sprintf("/dev/sd%c", "bcdefghij"[i])
		e.dev[d.slot] = dev
		e.disks.AddDisk(dev, disk.Disk{
			Serial: d.serial(), Size: d.size, Filesystem: fsType, FSDevice: dev + "1",
			FSUUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1),
		})
		e.cleanLog(dev+"1", fsType)
		if d.kind == "data" {
			e.trees[dev+"1"] = tree{files: map[string]string{"media/" + d.slot + ".txt": "hello " + d.slot}}
		}
	}
	e.mounter.OnMount = func(where string) error {
		last := e.mounter.Mounts[len(e.mounter.Mounts)-1]
		e.trees[last.Device].populate(t, where)
		return nil
	}
	e.scanner = &Scanner{Disks: e.disks, Runner: e.runner, Mounter: e.mounter, Dir: e.dir, UIDOwner: noUID, Now: fixedNow, FreeSpace: func(string) (int64, error) { return 1 << 40, nil }}
	return e
}

// btrfsSuperOut is what `btrfs inspect-internal dump-super` prints for a
// superblock of a filesystem spanning devices devices with the given log_root.
func btrfsSuperOut(devices int, logRoot uint64) []byte {
	return []byte(fmt.Sprintf("superblock: bytenr=65536, device=/dev/x\n---\ncsum_type\t\t1 (xxhash64)\nlog_root\t\t%d\nlog_root_transid\t0\nlog_root_level\t\t0\nnum_devices\t\t%d\nsectorsize\t\t4096\n", logRoot, devices))
}

// dumpe2fsOut is what `dumpe2fs -h` prints for an ext4 filesystem with the
// given feature list.
func dumpe2fsOut(features string) []byte {
	return []byte("Filesystem volume name:   <none>\nFilesystem features:      " + features + "\nFilesystem state:         clean\n")
}

func (e *dataEnv) scriptBtrfs(dev string, devices int, logRoot uint64) {
	e.runner.Script("btrfs", []string{"inspect-internal", "dump-super", dev}, btrfsSuperOut(devices, logRoot), nil)
}

func (e *dataEnv) scriptExt4(dev, features string) {
	e.runner.Script("dumpe2fs", []string{"-h", dev}, dumpe2fsOut(features), nil)
}

// cleanLog scripts the superblock inspection of dev as a filesystem that was
// closed cleanly.
func (e *dataEnv) cleanLog(dev, fsType string) {
	switch fsType {
	case "btrfs":
		e.scriptBtrfs(dev, 1, 0)
	case "ext4":
		e.scriptExt4(dev, "has_journal ext_attr dir_index filetype extent 64bit flex_bg")
	}
}

// setDisk changes what the inventory reports for a slot's disk.
func (e *dataEnv) setDisk(slot string, change func(*disk.Disk)) {
	e.t.Helper()
	list, _ := e.disks.List(context.Background())
	for _, d := range list {
		if d.Device == e.dev[slot] {
			change(&d)
			e.disks.AddDisk(d.Device, d)
			return
		}
	}
	e.t.Fatalf("no disk for %s", slot)
}

func (e *dataEnv) scan() *Report {
	e.t.Helper()
	r, err := e.scanE()
	if err != nil {
		e.t.Fatalf("Scan: %v", err)
	}
	return r
}

func (e *dataEnv) scanE() (*Report, error) {
	for _, f := range e.baselineFiles() {
		_ = os.Remove(filepath.Join(e.dir, f))
	}
	return e.scanner.Scan(context.Background(), openZipBytes(e.t, zipOf(e.t, e.files, false)), e.opts)
}

func (e *dataEnv) mountedDevices() []string {
	var out []string
	for _, m := range e.mounter.Mounts {
		out = append(out, m.Device)
	}
	return out
}

func (e *dataEnv) ran(name string, args ...string) bool {
	for _, c := range e.runner.Calls() {
		if c.Name == name && strings.Join(c.Args, " ") == strings.Join(args, " ") {
			return true
		}
	}
	return false
}

// assertNothingLeft fails when a mount, a scratch directory or a partial
// baseline file outlived the scan.
func (e *dataEnv) assertNothingLeft() {
	e.t.Helper()
	if p := e.mounter.MountedPaths(); len(p) != 0 {
		e.t.Errorf("still mounted after the scan: %v", p)
	}
	entries, _ := os.ReadDir(e.dir)
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), tmpPrefix) {
			e.t.Errorf("a scratch file or directory is left: %s", ent.Name())
		}
	}
}

func (e *dataEnv) baselineFiles() []string {
	entries, _ := os.ReadDir(e.dir)
	var out []string
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), baselinePrefix) {
			out = append(out, ent.Name())
		}
	}
	return out
}

func rowFor(r *Report, check, subject string) (Row, bool) {
	for _, row := range r.Rows {
		if row.Check == check && row.Subject == subject {
			return row, true
		}
	}
	return Row{}, false
}

func hasRowText(r *Report, check string, st Status, subject, sub string) bool {
	for _, row := range r.Rows {
		if row.Check == check && row.Status == st && row.Subject == subject && strings.Contains(row.Detail, sub) {
			return true
		}
	}
	return false
}

// A real parity disk reports an XFS signature, and the scan takes its role from
// its slot: it is never mounted and never checked, on #74's primary fixture.
func TestDataDisks_AParitySlotIsNeverMountedOrChecked(t *testing.T) {
	e := newDataEnv(t, primary)
	r := e.scan()

	parity := e.dev["parity"]
	for _, c := range e.runner.Calls() {
		for _, a := range c.Args {
			if strings.HasPrefix(a, parity) {
				t.Errorf("the parity disk was checked: %s %v", c.Name, c.Args)
			}
		}
	}
	for _, d := range e.mountedDevices() {
		if strings.HasPrefix(d, parity) {
			t.Errorf("the parity disk was mounted: %s", d)
		}
	}
	want := []string{e.dev["disk1"] + "1", e.dev["disk2"] + "1", e.dev["disk3"] + "1"}
	if got := e.mountedDevices(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("mounted %v, want exactly the three data disks %v", got, want)
	}
	if !hasRowText(r, CheckDataDisks, StatusInfo, "parity", "never mounted or checked") {
		t.Errorf("no row says the parity slot is left alone: %+v", rowsFor(r, CheckDataDisks))
	}
	e.assertNothingLeft()
}

// Without disks.ini no slot has a role, so no disk is mounted or checked.
func TestDataDisks_WithoutDisksININoDiskIsTreatedAsData(t *testing.T) {
	e := newDataEnv(t, primary)
	delete(e.files, "config/hoserva/disks.ini")
	r := e.scan()
	if len(e.mounter.Mounts) != 0 || len(e.runner.Calls()) != 0 {
		t.Errorf("mounted %v and ran %v without disks.ini", e.mounter.Mounts, e.runner.Calls())
	}
	if !hasRowText(r, CheckDataDisks, StatusInfo, "", "no disk is known to be a data disk") {
		t.Errorf("no row says no disk is treated as data: %+v", rowsFor(r, CheckDataDisks))
	}
	if r.Baseline != nil {
		t.Errorf("a baseline was recorded without roles: %+v", r.Baseline)
	}
}

// Only read-only tools are run against a source disk, and every one of them is
// the exact check Q23 names.
func TestDataDisks_EveryCommandRunIsAReadOnlyCheck(t *testing.T) {
	e := newDataEnv(t, "unraid-6.12-xfs-single-parity")
	e.scan()
	allowed := map[string]bool{"xfs_repair -n": true, "e2fsck -n": true, "btrfs check --readonly": true, "btrfs inspect-internal": true, "dumpe2fs -h": true}
	calls := e.runner.Calls()
	if len(calls) != 3 {
		t.Fatalf("ran %v, want one check per data disk", calls)
	}
	for _, c := range calls {
		key := c.Name
		if len(c.Args) > 0 {
			key += " " + c.Args[0]
		}
		if !allowed[key] {
			t.Errorf("ran %s %v, which is not one of the read-only checks", c.Name, c.Args)
		}
	}
	for _, m := range e.mounter.Mounts {
		if m.FSType != "xfs" {
			t.Errorf("mounted %v as %s", m, m.FSType)
		}
	}
}

// Each filesystem is checked by its own tool, on the node udev reports for it.
func TestDataDisks_EachFilesystemIsCheckedByItsOwnReadOnlyTool(t *testing.T) {
	e := newDataEnv(t, "unraid-6.12-xfs-single-parity")
	e.files["config/disk.cfg"] = []byte("startArray=\"yes\"\ndefaultFsType=\"xfs\"\ndiskIdSlot.0=\"-\"\ndiskIdSlot.1=\"-\"\ndiskFsType.1=\"xfs\"\ndiskIdSlot.2=\"-\"\ndiskFsType.2=\"ext4\"\ndiskIdSlot.3=\"-\"\ndiskFsType.3=\"btrfs\"\n")
	e.setDisk("disk2", func(d *disk.Disk) { d.Filesystem = "ext4" })
	e.setDisk("disk3", func(d *disk.Disk) { d.Filesystem = "btrfs" })
	e.cleanLog(e.dev["disk2"]+"1", "ext4")
	e.cleanLog(e.dev["disk3"]+"1", "btrfs")
	r := e.scan()
	for _, want := range [][]string{
		{"xfs_repair", "-n", e.dev["disk1"] + "1"},
		{"e2fsck", "-n", e.dev["disk2"] + "1"},
		{"btrfs", "check", "--readonly", e.dev["disk3"] + "1"},
	} {
		if !e.ran(want[0], want[1:]...) {
			t.Errorf("did not run %v: ran %v", want, e.runner.Calls())
		}
	}
	for _, row := range r.Rows {
		if row.Status == StatusRefuse {
			t.Errorf("unexpected refusal: %+v", row)
		}
	}
	var fsTypes []string
	for _, m := range e.mounter.Mounts {
		fsTypes = append(fsTypes, m.FSType)
	}
	if strings.Join(fsTypes, ",") != "xfs,ext4,btrfs" {
		t.Errorf("mounted as %v, want xfs, ext4, btrfs", fsTypes)
	}
}

func TestClassifyFilesystem(t *testing.T) {
	for name, tc := range map[string]struct {
		flash, slot, udev string
		fs                disk.FilesystemType
		refusal           string
	}{
		"xfs both sides":             {"xfs", "", "xfs", disk.XFS, ""},
		"btrfs":                      {"btrfs", "", "btrfs", disk.BTRFS, ""},
		"ext4":                       {"ext4", "", "ext4", disk.EXT4, ""},
		"flash silent, device xfs":   {"", "", "xfs", disk.XFS, ""},
		"auto, the slot says xfs":    {"auto", "xfs", "xfs", disk.XFS, ""},
		"flash upper case":           {"XFS", "", "xfs", disk.XFS, ""},
		"luks in the flash":          {"luks:xfs", "", "xfs", "", "encrypted"},
		"luks on the device":         {"xfs", "", "crypto_LUKS", "", "encrypted"},
		"luks on both":               {"luks:btrfs", "", "crypto_LUKS", "", "encrypted"},
		"zfs in the flash":           {"zfs", "", "xfs", "", "ZFS"},
		"zfs member on the device":   {"xfs", "", "zfs_member", "", "ZFS"},
		"zfs encrypted in the flash": {"zfs - encrypted", "", "", "", "encrypted"},
		"disagreement":               {"xfs", "", "ext4", "", "does not guess"},
		"flash btrfs, device xfs":    {"btrfs", "", "xfs", "", "does not guess"},
		"no filesystem":              {"xfs", "", "", "", "no filesystem"},
		"ntfs on the device":         {"", "", "ntfs", "", "does not adopt"},
		"reiserfs in the flash":      {"reiserfs", "", "xfs", "", "does not adopt"},
		"unknown flash value":        {"foo", "", "xfs", "", "does not adopt"},
	} {
		got := classifyFilesystem(tc.flash, tc.slot, tc.udev)
		if got.fs != tc.fs || (tc.refusal == "") != (got.refusal == "") || !strings.Contains(got.refusal, tc.refusal) {
			t.Errorf("%s: classifyFilesystem(%q, %q, %q) = %+v, want fs %q refusal containing %q", name, tc.flash, tc.slot, tc.udev, got, tc.fs, tc.refusal)
		}
	}
}

// A refusal names the disk and says why, and the scan goes on with the others:
// they are adopted and baselined, and the refused disk is never mounted.
func TestDataDisks_ARefusedDiskIsNamedAndTheRestProceed(t *testing.T) {
	type refusal struct {
		check, text string
		prep        func(e *dataEnv)
	}
	for name, tc := range map[string]refusal{
		"zfs": {CheckDataDisks, "ZFS", func(e *dataEnv) {
			e.setDisk("disk2", func(d *disk.Disk) { d.Filesystem = "zfs_member" })
		}},
		"encrypted": {CheckDataDisks, "encrypted", func(e *dataEnv) {
			e.setDisk("disk2", func(d *disk.Disk) { d.Filesystem = "crypto_LUKS" })
		}},
		"disagreement": {CheckDataDisks, "does not guess", func(e *dataEnv) {
			e.setDisk("disk2", func(d *disk.Disk) { d.Filesystem = "ext4" })
		}},
		"no filesystem uuid": {CheckDataDisks, "mounts by filesystem UUID", func(e *dataEnv) {
			e.setDisk("disk2", func(d *disk.Disk) { d.FSUUID = "" })
		}},
		"duplicate uuid": {CheckDataDisks, "also on", func(e *dataEnv) {
			e.setDisk("disk2", func(d *disk.Disk) {
				list, _ := e.disks.List(context.Background())
				for _, o := range list {
					if o.Device == e.dev["disk3"] {
						d.FSUUID = strings.ToUpper(o.FSUUID)
					}
				}
			})
		}},
		"integrity check fails": {CheckIntegrity, "read-only xfs check failed", func(e *dataEnv) {
			e.runner.Script("xfs_repair", []string{"-n", e.dev["disk2"] + "1"}, nil, errors.New("exit status 1: ERROR: The filesystem has valuable metadata changes in a log which needs to be replayed"))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newDataEnv(t, primary)
			tc.prep(e)
			r := e.scan()
			if !hasRowText(r, tc.check, StatusRefuse, "disk2", tc.text) {
				t.Errorf("no %s refuse row for disk2 containing %q: %+v", tc.check, tc.text, r.Rows)
			}
			if row, _ := rowFor(r, tc.check, "disk2"); !strings.Contains(row.Detail, "disk2 is not adopted") {
				t.Errorf("the row does not say the disk is not adopted: %q", row.Detail)
			}
			if r.Verdict != VerdictNoGo {
				t.Errorf("verdict = %s, want no_go", r.Verdict)
			}
			for _, d := range e.mountedDevices() {
				if d == e.dev["disk2"]+"1" {
					t.Errorf("the refused disk was mounted")
				}
			}
			adopted := []string{"disk1", "disk3"}
			if name == "duplicate uuid" {
				// Both holders of the UUID are refused: nothing says which is which.
				adopted = []string{"disk1"}
				if !hasRowText(r, CheckDataDisks, StatusRefuse, "disk3", "also on") {
					t.Errorf("the other holder of the UUID was not refused: %+v", rowsFor(r, CheckDataDisks))
				}
			}
			for _, slot := range adopted {
				if !hasRowText(r, CheckIntegrity, StatusPass, slot, "is clean") || !hasRowText(r, CheckDataDisks, StatusPass, slot, "single filesystem") {
					t.Errorf("%s was not adopted after disk2 was refused: %+v", slot, r.Rows)
				}
			}
			if r.Baseline == nil || len(r.Baseline.Disks) != len(adopted) {
				t.Fatalf("baseline = %+v, want the %d adopted disks", r.Baseline, len(adopted))
			}
			for _, d := range r.Baseline.Disks {
				if d.Slot == "disk2" {
					t.Errorf("the refused disk is in the baseline")
				}
			}
			e.assertNothingLeft()
		})
	}
}

// A dirty XFS log is refused with the way out, and is never replayed.
func TestDataDisks_ADirtyXFSLogIsRefusedWithoutReplay(t *testing.T) {
	e := newDataEnv(t, primary)
	e.runner.Script("xfs_repair", []string{"-n", e.dev["disk2"] + "1"}, nil, errors.New("exit status 1: ERROR: The filesystem has valuable metadata changes in a log which needs to be replayed. Mount the filesystem to replay the log"))
	r := e.scan()
	row, _ := rowFor(r, CheckIntegrity, "disk2")
	if row.Status != StatusRefuse || !strings.Contains(row.Detail, "stop the array cleanly") || !strings.Contains(row.Detail, "never replays a log") {
		t.Errorf("row = %+v", row)
	}
	for _, m := range e.mounter.Mounts {
		if m.Device == e.dev["disk2"]+"1" {
			t.Errorf("the disk with a dirty log was mounted: %+v", m)
		}
	}
}

// Damage that is not an unreplayed log is refused without advice about stopping
// the array.
func TestDataDisks_AnXFSFailureThatIsNotALogGetsNoLogAdvice(t *testing.T) {
	e := newDataEnv(t, primary)
	e.runner.Script("xfs_repair", []string{"-n", e.dev["disk2"] + "1"}, nil, errors.New("exit status 1: bad magic number in free space btree block"))
	r := e.scan()
	row, _ := rowFor(r, CheckIntegrity, "disk2")
	if row.Status != StatusRefuse || !strings.Contains(row.Detail, "bad magic number") || strings.Contains(row.Detail, "stop the array cleanly") {
		t.Errorf("row = %+v", row)
	}
	if strings.Contains(row.Detail, "refusing to adopt: ") {
		t.Errorf("the tool's message is quoted with the check's own wrapper: %q", row.Detail)
	}
}

func TestDataDisks_MultiDeviceBtrfsIsRefusedByName(t *testing.T) {
	e := newDataEnv(t, primary)
	e.files["config/disk.cfg"] = []byte("startArray=\"yes\"\ndefaultFsType=\"xfs\"\ndiskIdSlot.0=\"-\"\ndiskIdSlot.1=\"-\"\ndiskFsType.1=\"xfs\"\ndiskIdSlot.2=\"-\"\ndiskFsType.2=\"btrfs\"\ndiskIdSlot.3=\"-\"\ndiskFsType.3=\"xfs\"\n")
	e.setDisk("disk2", func(d *disk.Disk) { d.Filesystem = "btrfs" })
	e.scriptBtrfs(e.dev["disk2"]+"1", 2, 0)
	r := e.scan()
	if !hasRowText(r, CheckDataDisks, StatusRefuse, "disk2", "one of 2 devices of a btrfs filesystem") {
		t.Errorf("rows: %+v", rowsFor(r, CheckDataDisks))
	}
	if e.ran("btrfs", "check", "--readonly", e.dev["disk2"]+"1") {
		t.Errorf("a multi-device btrfs member was checked as if it were a filesystem of its own")
	}
	// A show that cannot say how many devices there are is not a pass.
	e2 := newDataEnv(t, primary)
	e2.files["config/disk.cfg"] = e.files["config/disk.cfg"]
	e2.setDisk("disk2", func(d *disk.Disk) { d.Filesystem = "btrfs" })
	e2.runner.Script("btrfs", []string{"inspect-internal", "dump-super", e2.dev["disk2"] + "1"}, nil, errors.New("exit status 1"))
	r2 := e2.scan()
	if !hasRowText(r2, CheckDataDisks, StatusRefuse, "disk2", "could not be shown to be a single-device btrfs") {
		t.Errorf("rows: %+v", rowsFor(r2, CheckDataDisks))
	}
}

// setFS makes slot's disk a filesystem of fsType on both the flash and the
// device.
func (e *dataEnv) setFS(slot string, n int, fsType string) {
	e.files["config/disk.cfg"] = []byte(strings.Replace(string(e.files["config/disk.cfg"]), fmt.Sprintf("diskFsType.%d=\"xfs\"", n), fmt.Sprintf("diskFsType.%d=\"%s\"", n, fsType), 1))
	e.setDisk(slot, func(d *disk.Disk) { d.Filesystem = fsType })
}

// A btrfs disk whose superblock records a tree log would be replayed by a plain
// read-only mount, which writes to the disk: it is refused and never mounted.
func TestDataDisks_ABtrfsDiskWithAPendingTreeLogIsRefusedAndNeverMounted(t *testing.T) {
	e := newDataEnv(t, primary)
	e.setFS("disk2", 2, "btrfs")
	e.scriptBtrfs(e.dev["disk2"]+"1", 1, 31391744)
	r := e.scan()
	row, _ := rowFor(r, CheckIntegrity, "disk2")
	if row.Status != StatusRefuse || !strings.Contains(row.Detail, "log_root 31391744") || !strings.Contains(row.Detail, "stop the array cleanly") || !strings.Contains(row.Detail, "never replays a log") {
		t.Errorf("row = %+v", row)
	}
	if e.ran("btrfs", "check", "--readonly", e.dev["disk2"]+"1") {
		t.Errorf("the disk was checked after it was refused for its log")
	}
	for _, m := range e.mounter.Mounts {
		if m.Device == e.dev["disk2"]+"1" {
			t.Errorf("the disk with a pending tree log was mounted: %+v", m)
		}
	}
	if row, _ := rowFor(r, CheckIntegrity, "disk1"); row.Status != StatusPass {
		t.Errorf("the other disks were not carried on with: %+v", row)
	}
}

// A superblock that does not say whether it has a tree log is not shown clean.
func TestDataDisks_ABtrfsSuperblockWithoutALogRootIsRefused(t *testing.T) {
	e := newDataEnv(t, primary)
	e.setFS("disk2", 2, "btrfs")
	e.runner.Script("btrfs", []string{"inspect-internal", "dump-super", e.dev["disk2"] + "1"}, []byte("superblock: bytenr=65536\n---\nnum_devices\t\t1\n"), nil)
	r := e.scan()
	if !hasRowText(r, CheckDataDisks, StatusRefuse, "disk2", "whether it has a tree log") {
		t.Errorf("rows: %+v", rowsFor(r, CheckDataDisks))
	}
	for _, m := range e.mounter.Mounts {
		if m.Device == e.dev["disk2"]+"1" {
			t.Errorf("the disk was mounted: %+v", m)
		}
	}
}

// e2fsck -n exits 0 on an ext4 filesystem whose journal was never replayed, and
// a mount that does not load the journal then shows none of what is only in it:
// the disk is refused, never recorded with a hole in it.
func TestDataDisks_AnExt4DiskWithAnUnreplayedJournalIsRefusedAndNeverMounted(t *testing.T) {
	e := newDataEnv(t, primary)
	e.setFS("disk2", 2, "ext4")
	e.scriptExt4(e.dev["disk2"]+"1", "has_journal needs_recovery ext_attr dir_index filetype extent")
	r := e.scan()
	row, _ := rowFor(r, CheckIntegrity, "disk2")
	if row.Status != StatusRefuse || !strings.Contains(row.Detail, "needs_recovery") || !strings.Contains(row.Detail, "stop the array cleanly") || !strings.Contains(row.Detail, "never replays a log") {
		t.Errorf("row = %+v", row)
	}
	for _, m := range e.mounter.Mounts {
		if m.Device == e.dev["disk2"]+"1" {
			t.Errorf("the disk with an unreplayed journal was mounted: %+v", m)
		}
	}
	if row, _ := rowFor(r, CheckIntegrity, "disk1"); row.Status != StatusPass {
		t.Errorf("the other disks were not carried on with: %+v", row)
	}
}

// An inspection that fails, or whose output has no feature list, is not read as
// a clean journal.
func TestDataDisks_AnExt4JournalThatCannotBeInspectedIsRefused(t *testing.T) {
	for name, script := range map[string]func(e *dataEnv, dev string){
		"the command fails": func(e *dataEnv, dev string) {
			e.runner.Script("dumpe2fs", []string{"-h", dev}, nil, errors.New("exit status 1"))
		},
		"no feature list": func(e *dataEnv, dev string) {
			e.runner.Script("dumpe2fs", []string{"-h", dev}, []byte("Filesystem volume name:   <none>\n"), nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newDataEnv(t, primary)
			e.setFS("disk2", 2, "ext4")
			script(e, e.dev["disk2"]+"1")
			r := e.scan()
			if !hasRowText(r, CheckIntegrity, StatusRefuse, "disk2", "could not be shown to be clean") {
				t.Errorf("rows: %+v", rowsFor(r, CheckIntegrity))
			}
			for _, m := range e.mounter.Mounts {
				if m.Device == e.dev["disk2"]+"1" {
					t.Errorf("the disk was mounted: %+v", m)
				}
			}
		})
	}
}

// A pool device is read through the same mount, so a log it has pending means
// it is not read at all: the report warns and nothing is mounted.
func TestDiskReader_APoolDeviceWithAPendingLogIsNotRead(t *testing.T) {
	for name, set := range map[string]func(e *dataEnv, dev string){
		"btrfs tree log":      func(e *dataEnv, dev string) { e.scriptBtrfs(dev, 1, 4096) },
		"ext4 needs_recovery": func(e *dataEnv, dev string) { e.scriptExt4(dev, "has_journal needs_recovery extent") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newDataEnv(t, primary)
			cache := e.dev["cache"] + "1"
			if strings.HasPrefix(name, "ext4") {
				e.setDisk("cache", func(d *disk.Disk) { d.Filesystem = "ext4" })
			}
			set(e, cache)
			e.trees[cache] = tree{files: map[string]string{"appdata/x": "x"}}
			e.scanner.Dirs = &DiskReader{Mounter: e.mounter, Runner: e.runner, Dir: e.dir}
			r := e.scan()
			for _, m := range e.mounter.Mounts {
				if m.Device == cache {
					t.Errorf("the pool device with a pending log was mounted: %+v", m)
				}
			}
			warned := false
			for _, row := range rowsFor(r, CheckShares) {
				if row.Status == StatusWarn && strings.Contains(row.Detail, "pool cache") && strings.Contains(row.Detail, "stop the array cleanly") {
					warned = true
				}
			}
			if !warned {
				t.Errorf("the report does not say the cache was not read: %+v", rowsFor(r, CheckShares))
			}
		})
	}
}

// Without a runner to inspect the log, an ext4 or btrfs device is not read.
func TestDiskReader_WithoutARunnerAnExt4OrBtrfsDeviceIsNotRead(t *testing.T) {
	e := newDataEnv(t, primary)
	reader := &DiskReader{Mounter: e.mounter, Dir: e.dir}
	list, _ := e.disks.List(ctx0)
	for _, d := range list {
		if d.Device != e.dev["cache"] {
			continue
		}
		if _, err := reader.TopLevelDirs(ctx0, d); err == nil {
			t.Errorf("a btrfs device was read with no way to inspect its log")
		}
	}
	if len(e.mounter.Mounts) != 0 {
		t.Errorf("mounted %+v", e.mounter.Mounts)
	}
}

// Unraid boot devices are recognised and left alone, and never refused as ZFS.
func TestDataDisks_BootDevicesAreRecognisedAndLeftAlone(t *testing.T) {
	e := newDataEnv(t, primary)
	e.disks.AddDisk("/dev/sdx", disk.Disk{Size: 8 << 30, Filesystem: "vfat", Label: "UNRAID", FSUUID: "AAAA-BBBB", FSDevice: "/dev/sdx1"})
	e.disks.AddDisk("/dev/sdy", disk.Disk{Size: 8 << 30, Filesystem: "vfat", Label: "EFI", FSUUID: "CCCC-DDDD", FSDevice: "/dev/sdy2", UnraidBoot: true})
	e.disks.AddDisk("/dev/sdz", disk.Disk{Size: 8 << 30, Filesystem: "zfs_member", FSUUID: "123456", FSDevice: "/dev/sdz3", UnraidBoot: true})
	r := e.scan()
	for _, dev := range []string{"/dev/sdx", "/dev/sdy", "/dev/sdz"} {
		row, ok := rowFor(r, CheckBootDevice, dev)
		if !ok || row.Status != StatusInfo || !strings.Contains(row.Detail, "Unraid boot device") {
			t.Errorf("%s: row = %+v, want an info row calling it an Unraid boot device", dev, row)
		}
	}
	for _, row := range r.Rows {
		if row.Status == StatusRefuse || strings.Contains(row.Detail, "ZFS") && row.Check == CheckDataDisks {
			t.Errorf("a boot device was refused: %+v", row)
		}
	}
	for _, d := range e.mountedDevices() {
		if strings.HasPrefix(d, "/dev/sdx") || strings.HasPrefix(d, "/dev/sdy") || strings.HasPrefix(d, "/dev/sdz") {
			t.Errorf("a boot device was mounted: %s", d)
		}
	}
}

// A data slot the machine matches to an Unraid boot device is refused as that,
// not as ZFS, and is never mounted.
func TestDataDisks_ASlotMatchedToABootDeviceIsRefusedAsOne(t *testing.T) {
	e := newDataEnv(t, primary)
	e.setDisk("disk2", func(d *disk.Disk) { d.UnraidBoot = true; d.Filesystem = "vfat" })
	r := e.scan()
	if !hasRowText(r, CheckBootDevice, StatusRefuse, "disk2", "Unraid boot device") {
		t.Errorf("rows: %+v", r.Rows)
	}
	for _, d := range e.mountedDevices() {
		if d == e.dev["disk2"]+"1" {
			t.Errorf("mounted the boot device")
		}
	}
}

// A data slot whose disk is the boot device the capture names is never read, even
// when nothing on the machine marks it as one.
func TestDataDisks_ASlotWhoseDiskIsTheCapturesBootDeviceIsNeverRead(t *testing.T) {
	e := newDataEnv(t, primary)
	e.files["config/hoserva/capture.json"] = []byte(`{"unraid_version":"6.12.15","boot":{"mode":"internal","filesystem":"zfs","devices":[{"name":"boot","serial":"disk2-hoserva-test","model":"m","size":"1"}]}}`)
	r := e.scan()
	if !hasRowText(r, CheckBootDevice, StatusRefuse, "disk2", "boot device the capture names") {
		t.Errorf("rows: %+v", rowsFor(r, CheckBootDevice))
	}
	for _, d := range e.mountedDevices() {
		if d == e.dev["disk2"]+"1" {
			t.Errorf("mounted the capture's boot device")
		}
	}
	if e.ran("xfs_repair", "-n", e.dev["disk2"]+"1") {
		t.Errorf("checked the capture's boot device")
	}
}

// The machine's own boot disk is never an array disk.
func TestDataDisks_TheMachinesOwnBootDiskIsNeverRead(t *testing.T) {
	e := newDataEnv(t, primary)
	e.setDisk("disk1", func(d *disk.Disk) { d.Boot = true })
	r := e.scan()
	if !hasRowText(r, CheckDataDisks, StatusRefuse, "disk1", "boots from") {
		t.Errorf("rows: %+v", rowsFor(r, CheckDataDisks))
	}
	for _, d := range e.mountedDevices() {
		if d == e.dev["disk1"]+"1" {
			t.Errorf("mounted this machine's boot disk")
		}
	}
	for _, c := range e.runner.Calls() {
		if strings.Contains(strings.Join(c.Args, " "), e.dev["disk1"]) {
			t.Errorf("checked this machine's boot disk: %v", c)
		}
	}
}

// A mount that fails refuses that disk and not the scan.
func TestDataDisks_AMountThatFailsRefusesThatDiskOnly(t *testing.T) {
	e := newDataEnv(t, primary)
	inner := e.mounter.OnMount
	e.mounter.OnMount = func(where string) error {
		last := e.mounter.Mounts[len(e.mounter.Mounts)-1]
		if last.Device == e.dev["disk2"]+"1" {
			return errors.New("input/output error")
		}
		return inner(where)
	}
	r := e.scan()
	if !hasRowText(r, CheckBaseline, StatusRefuse, "disk2", "could not be read completely") {
		t.Errorf("rows: %+v", rowsFor(r, CheckBaseline))
	}
	if r.Baseline == nil || len(r.Baseline.Disks) != 2 {
		t.Fatalf("baseline = %+v, want disk1 and disk3", r.Baseline)
	}
	e.assertNothingLeft()
}

// An entry that cannot be read leaves a hole in the baseline, so the disk is
// refused instead of being recorded as if it were whole.
func TestDataDisks_ADiskThatCannotBeReadCompletelyIsRefusedNotRecordedWhole(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory with no permissions")
	}
	e := newDataEnv(t, primary)
	inner := e.mounter.OnMount
	e.mounter.OnMount = func(where string) error {
		if err := inner(where); err != nil {
			return err
		}
		last := e.mounter.Mounts[len(e.mounter.Mounts)-1]
		if last.Device == e.dev["disk2"]+"1" {
			locked := filepath.Join(where, "media", "locked")
			if err := os.MkdirAll(locked, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o644); err != nil {
				return err
			}
			return os.Chmod(locked, 0)
		}
		return nil
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(e.dir, func(p string, d os.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	r := e.scan()
	if !hasRowText(r, CheckBaseline, StatusRefuse, "disk2", "could not be read completely") {
		t.Errorf("rows: %+v", rowsFor(r, CheckBaseline))
	}
	var entriesOfDisk2 int
	br := openOnlyBaseline(t, e, r)
	if err := br.Each(func(e BaselineEntry) error {
		if e.Disk == "disk2" {
			entriesOfDisk2++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if entriesOfDisk2 != 0 {
		t.Errorf("%d entries of the refused disk reached the baseline", entriesOfDisk2)
	}
}

func openOnlyBaseline(t *testing.T, e *dataEnv, r *Report) *BaselineReader {
	t.Helper()
	if r.Baseline == nil {
		t.Fatal("no baseline")
	}
	if files := e.baselineFiles(); len(files) != 1 || files[0] != r.Baseline.File {
		t.Fatalf("baseline files = %v, want only %s", files, r.Baseline.File)
	}
	br, err := OpenBaseline(filepath.Join(e.dir, r.Baseline.File))
	if err != nil {
		t.Fatal(err)
	}
	return br
}

// A source disk that cannot be released fails the scan: nothing may stay mounted.
func TestDataDisks_ADiskThatCannotBeUnmountedFailsTheScan(t *testing.T) {
	e := newDataEnv(t, primary)
	e.mounter.UnmountErr = errors.New("target is busy")
	_, err := e.scanE()
	if !errors.Is(err, ErrDiskRelease) {
		t.Fatalf("Scan error = %v, want ErrDiskRelease", err)
	}
	if got := e.baselineFiles(); len(got) != 0 {
		t.Errorf("a baseline was published by a scan that could not release its disk: %v", got)
	}
}

// A cancelled scan stops, releases its mount and leaves no baseline behind.
func TestDataDisks_ACancelledScanUnmountsAndLeavesNothing(t *testing.T) {
	e := newDataEnv(t, primary)
	ctx, cancel := context.WithCancel(context.Background())
	inner := e.mounter.OnMount
	e.mounter.OnMount = func(where string) error {
		if err := inner(where); err != nil {
			return err
		}
		if len(e.mounter.Mounts) == 2 {
			cancel()
		}
		return nil
	}
	_, err := e.scanner.Scan(ctx, openZipBytes(t, zipOf(t, e.files, false)), ScanOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan error = %v, want context.Canceled", err)
	}
	e.assertNothingLeft()
	if got := e.baselineFiles(); len(got) != 0 {
		t.Errorf("a cancelled scan left %v", got)
	}
	if n := len(e.mounter.Mounts); n != 2 {
		t.Errorf("mounted %d disks, want the scan to stop after the second", n)
	}
}

// A mount that cannot be released is reported even when the scan was cancelled as
// well: the cancellation does not hide a disk left mounted.
func TestDataDisks_ACancelledScanStillReportsADiskItCouldNotUnmount(t *testing.T) {
	e := newDataEnv(t, primary)
	ctx, cancel := context.WithCancel(context.Background())
	inner := e.mounter.OnMount
	e.mounter.OnMount = func(where string) error {
		if err := inner(where); err != nil {
			return err
		}
		e.mounter.UnmountErr = errors.New("target is busy")
		cancel()
		return nil
	}
	_, err := e.scanner.Scan(ctx, openZipBytes(t, zipOf(t, e.files, false)), ScanOptions{})
	if !errors.Is(err, ErrDiskRelease) {
		t.Fatalf("Scan error = %v, want ErrDiskRelease even though the scan was cancelled", err)
	}
}

func TestDataDisks_AFailedScanOfAnyKindLeavesNoMountBehind(t *testing.T) {
	for name, fail := range map[string]func(e *dataEnv){
		"a mount error":    func(e *dataEnv) { e.mounter.MountErr = errors.New("no such device") },
		"an unmount error": func(e *dataEnv) { e.mounter.UnmountErr = errors.New("busy") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newDataEnv(t, primary)
			fail(e)
			_, _ = e.scanE()
			if name == "a mount error" {
				e.assertNothingLeft()
			}
			if got := e.baselineFiles(); len(got) != 0 {
				t.Errorf("left %v", got)
			}
		})
	}
}

// The baseline records every file's size, symlinks and special files, and leaves
// hidden top-level directories out.
func TestBaseline_RecordsEveryEntryAndLeavesHiddenDirectoriesOut(t *testing.T) {
	e := newDataEnv(t, primary)
	sdc := e.dev["disk1"] + "1"
	e.trees[sdc] = tree{
		files: map[string]string{"media/a.txt": "aaa", "media/sub/b.txt": "bb", "docs/c.txt": "", ".Trash-0/gone.txt": "trash", "rootfile.txt": "r"},
		links: map[string]string{"media/link": "../docs/c.txt", "media/dirlink": "/etc"},
		fifos: []string{"media/pipe"},
		dirs:  []string{"empty", ".Trash-1000"},
	}
	r := e.scan()
	br := openOnlyBaseline(t, e, r)
	got := map[string]BaselineEntry{}
	if err := br.Each(func(en BaselineEntry) error {
		if en.Disk == "disk1" {
			got[en.name()] = en
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantKinds := map[string]string{
		"media/a.txt": KindFile, "media/sub/b.txt": KindFile, "docs/c.txt": KindFile, "rootfile.txt": KindFile,
		"media/link": KindSymlink, "media/dirlink": KindSymlink, "media/pipe": KindSpecial,
	}
	for p, k := range wantKinds {
		if got[p].Kind != k {
			t.Errorf("%s: kind %q, want %q (entries: %v)", p, got[p].Kind, k, keysOf(got))
		}
	}
	if len(got) != len(wantKinds) {
		t.Errorf("entries %v, want exactly %d", keysOf(got), len(wantKinds))
	}
	if got["media/a.txt"].Size != 3 || got["media/sub/b.txt"].Size != 2 || got["docs/c.txt"].Size != 0 {
		t.Errorf("sizes: %+v", got)
	}
	if got["media/link"].Target != "../docs/c.txt" || got["media/dirlink"].Target != "/etc" {
		t.Errorf("symlink targets: %+v %+v", got["media/link"], got["media/dirlink"])
	}
	if got["media/pipe"].Special != "fifo" {
		t.Errorf("special: %+v", got["media/pipe"])
	}
	var d1 DiskBaseline
	for _, d := range r.Baseline.Disks {
		if d.Slot == "disk1" {
			d1 = d
		}
	}
	if d1.Files != 4 || d1.Symlinks != 2 || d1.Special != 1 || d1.Bytes != 3+2+0+1 || d1.RootEntries != 1 {
		t.Errorf("disk1 totals = %+v", d1)
	}
	if strings.Join(d1.HiddenSkipped, ",") != ".Trash-0,.Trash-1000" {
		t.Errorf("hidden = %v", d1.HiddenSkipped)
	}
	shares := map[string]ShareTotals{}
	for _, s := range d1.Shares {
		shares[s.Share] = s
	}
	if s := shares["media"]; s.Files != 2 || s.Bytes != 5 || s.Symlinks != 2 || s.Special != 1 {
		t.Errorf("media = %+v", s)
	}
	if _, ok := shares[".Trash-0"]; ok {
		t.Errorf("a hidden directory is a share: %+v", d1.Shares)
	}
	// A symlink to a directory is recorded and never followed: /etc's files are
	// not in the baseline.
	for p := range got {
		if strings.HasPrefix(p, "media/dirlink/") {
			t.Errorf("a symlink was followed: %s", p)
		}
	}
}

func keysOf(m map[string]BaselineEntry) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Content hashes are the real sha256, for every small file.
func TestBaseline_HashesEverySmallFile(t *testing.T) {
	e := newDataEnv(t, primary)
	e.trees[e.dev["disk1"]+"1"] = tree{files: map[string]string{"media/a.txt": "abc", "media/empty": ""}}
	r := e.scan()
	br := openOnlyBaseline(t, e, r)
	got := map[string]string{}
	_ = br.Each(func(en BaselineEntry) error {
		if en.Disk == "disk1" {
			got[en.name()] = en.SHA256
		}
		return nil
	})
	if got["media/a.txt"] != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("sha256 of abc = %q", got["media/a.txt"])
	}
	if got["media/empty"] != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("sha256 of nothing = %q", got["media/empty"])
	}
}

// The sample of larger files is the lowest path hashes, the same on every run,
// and at least 200 of them.
func TestBaseline_TheLargeFileSampleIsDeterministic(t *testing.T) {
	e := newDataEnv(t, primary)
	big := map[string]int64{}
	var paths []string
	for i := 0; i < 300; i++ {
		p := fmt.Sprintf("media/big-%03d.bin", i)
		big[p] = DefaultSmallFileBytes + 1
		paths = append(paths, p)
	}
	e.trees[e.dev["disk1"]+"1"] = tree{big: big, files: map[string]string{"media/small.txt": "s"}}
	hashed := func() map[string]bool {
		r := e.scan()
		br := openOnlyBaseline(t, e, r)
		out := map[string]bool{}
		_ = br.Each(func(en BaselineEntry) error {
			if en.Disk == "disk1" && en.SHA256 != "" && en.Size > DefaultSmallFileBytes {
				out[en.name()] = true
			}
			return nil
		})
		return out
	}
	first := hashed()
	if len(first) != 200 {
		t.Fatalf("%d large files were hashed, want 200 (the minimum, of 300)", len(first))
	}
	sort.Slice(paths, func(i, j int) bool { return pathKey(paths[i]) < pathKey(paths[j]) })
	for _, p := range paths[:200] {
		if !first[p] {
			t.Errorf("%s is among the 200 lowest path hashes and was not hashed", p)
		}
	}
	// The rule is stored with the baseline.
	r := e.scan()
	if r.Baseline.Rule.Full || r.Baseline.Rule.SmallFileBytes != DefaultSmallFileBytes || r.Baseline.Rule.MinLarge != 200 || r.Baseline.Rule.LargeEvery != 100 {
		t.Errorf("rule = %+v", r.Baseline.Rule)
	}
	second := hashed()
	if fmt.Sprint(sortedSet(first)) != fmt.Sprint(sortedSet(second)) {
		t.Errorf("two scans of the same files chose different samples")
	}
}

func sortedSet(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestBaseline_FullChecksumsHashEveryFile(t *testing.T) {
	e := newDataEnv(t, primary)
	big := map[string]int64{}
	for i := 0; i < 5; i++ {
		big[fmt.Sprintf("media/big-%d.bin", i)] = DefaultSmallFileBytes + 1
	}
	e.trees[e.dev["disk1"]+"1"] = tree{big: big}
	e.opts.FullChecksums = true
	r := e.scan()
	if !r.Baseline.Rule.Full {
		t.Errorf("rule = %+v, want full", r.Baseline.Rule)
	}
	br := openOnlyBaseline(t, e, r)
	n, hashed := 0, 0
	_ = br.Each(func(en BaselineEntry) error {
		if en.Disk == "disk1" {
			n++
			if en.SHA256 != "" {
				hashed++
			}
		}
		return nil
	})
	if n != 5 || hashed != 5 {
		t.Errorf("%d of %d files hashed, want all", hashed, n)
	}
}

func TestSampleCount(t *testing.T) {
	for _, tc := range []struct{ n, want int64 }{{0, 0}, {1, 1}, {199, 199}, {300, 200}, {20000, 200}, {20001, 201}, {1000000, 10000}} {
		if got := sampleCount(tc.n, 100, 200); got != tc.want {
			t.Errorf("sampleCount(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

// More than 20000 larger files: the sample is 1%, still the lowest path hashes.
func TestSampleThreshold_PicksTheLowestOnePercent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "list")
	w, err := newEntryWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	var keys []uint64
	for i := 0; i < 30000; i++ {
		name := fmt.Sprintf("share/f-%d", i)
		keys = append(keys, pathKey(name))
		e := BaselineEntry{Kind: KindFile, Size: DefaultSmallFileBytes + 1}
		e.setName(name)
		if err := w.write(e.record()); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	threshold, found, err := sampleThreshold(path, sampleRule(false))
	if err != nil || !found {
		t.Fatalf("sampleThreshold = %d, %v, %v", threshold, found, err)
	}
	if threshold != keys[299] {
		t.Errorf("threshold = %d, want the 300th lowest key %d (1%% of 30000)", threshold, keys[299])
	}
}

// The same file path on two disks is recorded as a duplicate, and listed in the
// report, with the disks that hold it.
func TestBaseline_PathsOnMoreThanOneDiskAreRecordedAndListed(t *testing.T) {
	e := newDataEnv(t, primary)
	e.trees[e.dev["disk1"]+"1"] = tree{files: map[string]string{"media/dup.txt": "a", "media/only1.txt": "1", "docs/x/y.txt": "y1", "z.txt": "z"}}
	e.trees[e.dev["disk2"]+"1"] = tree{files: map[string]string{"media/dup.txt": "b", "media/only2.txt": "2", "docs/x/y.txt": "y2"}}
	e.trees[e.dev["disk3"]+"1"] = tree{files: map[string]string{"media/dup.txt": "c", "media/sub/deep.txt": "d"}, links: map[string]string{"docs/x/y.txt": "elsewhere"}}
	r := e.scan()
	if r.Baseline.Duplicates != 2 {
		t.Fatalf("duplicates = %d, want 2: %+v", r.Baseline.Duplicates, r.Baseline.DuplicateSample)
	}
	want := map[string]string{"media/dup.txt": "disk1,disk2,disk3", "docs/x/y.txt": "disk1,disk2,disk3"}
	for _, d := range r.Baseline.DuplicateSample {
		if strings.Join(d.Disks, ",") != want[d.Path] {
			t.Errorf("%s on %v, want %s", d.Path, d.Disks, want[d.Path])
		}
	}
	row, ok := rowFor(r, CheckBaseline, "")
	_ = row
	found := false
	for _, row := range rowsFor(r, CheckBaseline) {
		if row.Status == StatusFlag && strings.Contains(row.Detail, "2 paths exist on more than one disk") && strings.Contains(row.Detail, "media/dup.txt (on disk1, disk2, disk3)") {
			found = true
		}
	}
	if !found {
		t.Errorf("no flag row lists the duplicates (rows: %+v, %v)", rowsFor(r, CheckBaseline), ok)
	}
	br := openOnlyBaseline(t, e, r)
	var inFile []string
	if err := br.Duplicates(func(d DuplicatePath) error {
		inFile = append(inFile, d.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(inFile)
	if strings.Join(inFile, ",") != "docs/x/y.txt,media/dup.txt" {
		t.Errorf("the baseline records duplicates %v", inFile)
	}
}

// The merge relies on every disk's walk being in comparePaths order.
func TestComparePaths_AgreesWithTheWalkOrder(t *testing.T) {
	root := t.TempDir()
	names := []string{"a", "a.b", "a-b", "ab", "B", "é", "a b", "d/b", "d/b.c", "d/c", "d/sub/z", "d.x/y", "e/a", "e/a b"}
	for _, n := range names {
		p := filepath.Join(root, "share", filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list := filepath.Join(t.TempDir(), "list")
	w, err := newEntryWriter(list)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := walkDisk(context.Background(), root, "disk1", w, func(int64) {}); err != nil {
		t.Fatal(err)
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	var seen []string
	if err := readRecords(list, func(r record) error {
		seen = append(seen, r.P)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) < 8 {
		t.Fatalf("only %d entries: %v", len(seen), seen)
	}
	for i := 1; i < len(seen); i++ {
		if comparePaths(seen[i-1], seen[i]) >= 0 {
			t.Errorf("the walk visited %q before %q, but comparePaths does not put them in that order", seen[i-1], seen[i])
		}
	}
}

// A name that is not valid UTF-8 survives the baseline byte for byte.
func TestBaseline_NonUTF8NamesRoundTrip(t *testing.T) {
	e := newDataEnv(t, primary)
	odd := "media/caf\xe9.txt"
	e.trees[e.dev["disk1"]+"1"] = tree{files: map[string]string{odd: "x"}, links: map[string]string{"media/l": "t\xffarget"}}
	r := e.scan()
	br := openOnlyBaseline(t, e, r)
	var name, target string
	_ = br.Each(func(en BaselineEntry) error {
		if en.Disk == "disk1" && en.Kind == KindFile {
			name = en.name()
		}
		if en.Disk == "disk1" && en.Kind == KindSymlink {
			target = en.Target
			if en.RawTarget != nil {
				target = string(en.RawTarget)
			}
		}
		return nil
	})
	if name != odd {
		t.Errorf("name = %q, want %q", name, odd)
	}
	if target != "t\xffarget" {
		t.Errorf("target = %q", target)
	}
}

// A baseline cut short is never taken for a whole one.
func TestOpenBaseline_RefusesAFileThatIsCutShort(t *testing.T) {
	e := newDataEnv(t, primary)
	r := e.scan()
	path := filepath.Join(e.dir, r.Baseline.File)
	if _, err := OpenBaseline(path); err != nil {
		t.Fatalf("a whole baseline: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(t.TempDir(), "cut")
	if err := os.WriteFile(cut, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBaseline(cut); err == nil {
		t.Error("a baseline cut in half was opened")
	}
	if _, err := OpenBaseline(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrNoBaseline) {
		t.Errorf("a missing baseline: %v, want ErrNoBaseline", err)
	}
}

// Progress is reported as the disks are read, and ends at the top.
func TestDataDisks_ProgressIsReportedPerDisk(t *testing.T) {
	e := newDataEnv(t, primary)
	var pcts []int
	var lines []string
	e.opts.Progress = func(p int, line string) {
		pcts = append(pcts, p)
		lines = append(lines, line)
	}
	e.scan()
	if len(pcts) == 0 || pcts[len(pcts)-1] != 100 {
		t.Fatalf("progress = %v, want it to end at 100", pcts)
	}
	for i := 1; i < len(pcts); i++ {
		if pcts[i] < pcts[i-1] {
			t.Errorf("progress went back: %v", pcts)
		}
	}
	joined := strings.Join(lines, "\n")
	for _, slot := range []string{"disk1", "disk2", "disk3"} {
		if !strings.Contains(joined, slot+": ") {
			t.Errorf("no progress line mentions %s:\n%s", slot, joined)
		}
	}
}

func TestContentSpace(t *testing.T) {
	e := newDataEnv(t, primary)
	r := e.scan()
	if !hasRowText(r, CheckContentSpace, StatusPass, "", "3 content-file copies can be placed") {
		t.Errorf("rows: %+v", rowsFor(r, CheckContentSpace))
	}

	// Dual parity wants four copies: the boot device, the cache, and two data
	// disks. With three of the four data disks refused only one is left.
	e = newDataEnv(t, "unraid-dual-parity")
	for _, slot := range []string{"disk2", "disk3", "disk4"} {
		e.setDisk(slot, func(d *disk.Disk) { d.Filesystem = "zfs_member" })
	}
	r = e.scan()
	if !hasRowText(r, CheckContentSpace, StatusFlag, "", "Q18 wants 4 content-file copies") {
		t.Errorf("rows: %+v", rowsFor(r, CheckContentSpace))
	}

	// Disks with no room for the file.
	e = newDataEnv(t, primary)
	e.scanner.FreeSpace = func(string) (int64, error) { return 10, nil }
	r = e.scan()
	if !hasRowText(r, CheckContentSpace, StatusFlag, "", "have room for") {
		t.Errorf("rows: %+v", rowsFor(r, CheckContentSpace))
	}

	// The boot device cannot hold the first copy.
	e = newDataEnv(t, primary)
	e.scanner.FreeSpace = func(path string) (int64, error) {
		if path == e.dir {
			return 10, nil
		}
		return 1 << 40, nil
	}
	r = e.scan()
	if !hasRowText(r, CheckContentSpace, StatusFlag, "", "boot device has 10 B free") {
		t.Errorf("rows: %+v", rowsFor(r, CheckContentSpace))
	}

	// Without a read data disk the check says it did not run.
	e = newDataEnv(t, primary)
	e.scanner.Runner = nil
	r = e.scan()
	if !hasRowText(r, CheckContentSpace, StatusInfo, "", "Not evaluated") {
		t.Errorf("rows: %+v", rowsFor(r, CheckContentSpace))
	}
}

// Without the means to read the data disks the scan says it did not.
func TestDataDisks_AScanThatCannotReadTheDisksSaysSo(t *testing.T) {
	e := newDataEnv(t, primary)
	e.scanner.Mounter = nil
	r := e.scan()
	if !hasRowText(r, CheckDataDisks, StatusWarn, "", "not set up to read the data disks") {
		t.Errorf("rows: %+v", rowsFor(r, CheckDataDisks))
	}
	if r.Verdict == VerdictGo {
		t.Errorf("verdict = go although no data disk was checked")
	}
}

// The directories found while the disks are read are what tells a share from the
// config of one that no longer exists: with every disk matched and read, a config
// with no directory anywhere is an orphan and is not imported.
func TestDataDisks_TheDirectoriesReadFromTheDisksFindOrphanShareConfigs(t *testing.T) {
	e := newDataEnv(t, primary)
	e.trees[e.dev["disk1"]+"1"] = tree{files: map[string]string{"media/a": "a"}, dirs: []string{"documents", ".Trash-0"}}
	e.trees[e.dev["cache"]+"1"] = tree{files: map[string]string{"appdata/x": "x"}}
	e.scanner.Dirs = &DiskReader{Mounter: e.mounter, Runner: e.runner, Dir: e.dir}
	r := e.scan()

	orphans := map[string]bool{}
	for _, row := range rowsFor(r, CheckShares) {
		if row.Status == StatusInfo && strings.Contains(row.Detail, "is an orphan") {
			orphans[row.Subject] = true
		}
	}
	for _, name := range []string{"backup", "domains", "isos", "oldstuff", "system"} {
		if !orphans[name] {
			t.Errorf("%s has no directory on any disk and was not called an orphan: %+v", name, rowsFor(r, CheckShares))
		}
	}
	for _, name := range []string{"media", "documents", "appdata"} {
		if orphans[name] {
			t.Errorf("%s has a directory and was called an orphan", name)
		}
	}
	kept := map[string]bool{}
	for _, sh := range r.Import.Shares {
		kept[sh.Name] = true
	}
	if !kept["media"] || !kept["documents"] || !kept["appdata"] || kept["backup"] {
		t.Errorf("imported shares = %v", kept)
	}
	if row, _ := rowFor(r, CheckShares, "media"); !strings.Contains(row.Detail, "directory on disk1") {
		t.Errorf("media row = %+v", row)
	}
	e.assertNothingLeft()
}

// With a disk refused none of its directories were read, so no config is called
// an orphan: its share may have its only directory there.
func TestDataDisks_ARefusedDiskKeepsEveryShareConfig(t *testing.T) {
	e := newDataEnv(t, primary)
	e.scanner.Dirs = &DiskReader{Mounter: e.mounter, Runner: e.runner, Dir: e.dir}
	e.setDisk("disk2", func(d *disk.Disk) { d.Filesystem = "zfs_member" })
	r := e.scan()
	warned := false
	for _, row := range rowsFor(r, CheckShares) {
		if strings.Contains(row.Detail, "is an orphan") {
			t.Errorf("an orphan was called with a disk unread: %+v", row)
		}
		if row.Status == StatusWarn && strings.Contains(row.Detail, "disk2: it is not adopted, so its directories were not read") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("the report does not say disk2's directories were not read: %+v", rowsFor(r, CheckShares))
	}
	if len(r.Import.Shares) != 8 {
		t.Errorf("%d share configs kept, want all 8", len(r.Import.Shares))
	}
}

// With one data disk and single parity, Unraid's parity is a copy of the data
// disk, so the parity partition carries its filesystem UUID. The data disk is
// adopted through its own by-id link and the scan says so; the parity disk is
// still never mounted. Without the link the data disk is refused, because
// nothing but the UUID could tell the two apart.
func TestDataDisks_AParityDiskHoldingACopyOfADataDisksUUID(t *testing.T) {
	copyUUID := func(e *dataEnv) {
		list, _ := e.disks.List(context.Background())
		for _, d := range list {
			if d.Device == e.dev["disk1"] {
				e.setDisk("parity", func(p *disk.Disk) { p.FSUUID = d.FSUUID })
			}
		}
	}
	t.Run("with an identity link", func(t *testing.T) {
		e := newDataEnv(t, primary)
		copyUUID(e)
		e.setDisk("disk1", func(d *disk.Disk) { d.ByIDName, d.FSByIDName = "ata-EX_disk1", "ata-EX_disk1-part1" })
		r := e.scan()
		if !hasRowText(r, CheckDataDisks, StatusPass, "disk1", "single filesystem") || !hasRowText(r, CheckIntegrity, StatusPass, "disk1", "is clean") {
			t.Errorf("disk1 was not adopted: %+v", rowsFor(r, CheckDataDisks))
		}
		if !hasRowText(r, CheckDataDisks, StatusInfo, "disk1", "never by UUID") {
			t.Errorf("no row says disk1 is mounted through its own link: %+v", rowsFor(r, CheckDataDisks))
		}
		for _, d := range e.mountedDevices() {
			if d == e.dev["parity"]+"1" {
				t.Errorf("the parity disk was mounted: %v", e.mountedDevices())
			}
		}
		if r.Verdict == VerdictNoGo {
			t.Errorf("verdict = no_go: %+v", r.Rows)
		}
	})
	t.Run("without one", func(t *testing.T) {
		e := newDataEnv(t, primary)
		copyUUID(e)
		r := e.scan()
		if !hasRowText(r, CheckDataDisks, StatusRefuse, "disk1", "no /dev/disk/by-id link") || r.Verdict != VerdictNoGo {
			t.Errorf("disk1 was not refused: %+v (verdict %s)", rowsFor(r, CheckDataDisks), r.Verdict)
		}
		if review := r.Review.Disks; len(review) == 0 {
			t.Fatal("no review")
		} else {
			found := false
			for _, d := range review {
				if d.Slot == "disk1" && d.RefusalCode == RefuseDuplicateUUID {
					found = true
				}
			}
			if !found {
				t.Errorf("disk1 has no duplicate_uuid refusal in the review: %+v", review)
			}
		}
		for _, d := range e.mountedDevices() {
			if d == e.dev["disk1"]+"1" || d == e.dev["parity"]+"1" {
				t.Errorf("a disk of the ambiguous pair was mounted: %v", e.mountedDevices())
			}
		}
	})
}
