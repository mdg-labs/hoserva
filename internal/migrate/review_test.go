package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func reviewDisk(t *testing.T, r *Report, slot string) ReviewDisk {
	t.Helper()
	if r.Review == nil {
		t.Fatal("the report has no review")
	}
	for _, d := range r.Review.Disks {
		if d.Slot == slot {
			return d
		}
	}
	t.Fatalf("no review disk for slot %q in %+v", slot, r.Review.Disks)
	return ReviewDisk{}
}

func reviewSlots(r *Report) []string {
	var out []string
	for _, d := range r.Review.Disks {
		out = append(out, d.Slot)
	}
	return out
}

// Every disk the capture names is one row of the table, carrying what its
// mapping row says as data, and the parity disk is parity by its slot though its
// device reports XFS.
func TestReview_TheDiskTableHoldsEverySlotOnce(t *testing.T) {
	e := newDataEnv(t, primary)
	r := e.scan()

	if got, want := reviewSlots(r), []string{"parity", "disk1", "disk2", "disk3", "pool cache"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("slots = %v, want %v", got, want)
	}
	for _, tc := range []struct {
		slot       string
		unraid     UnraidRole
		proposed   ProposedRole
		number     int
		filesystem string
	}{
		{"parity", UnraidParity, ProposeParity, 0, "xfs"},
		{"disk1", UnraidData, ProposeData, 1, "xfs"},
		{"disk2", UnraidData, ProposeData, 2, "xfs"},
		{"disk3", UnraidData, ProposeData, 3, "xfs"},
		{"pool cache", UnraidCache, ProposeCache, 0, "btrfs"},
	} {
		d := reviewDisk(t, r, tc.slot)
		var spec fixtureDisk
		for _, s := range e.spec.disks {
			if s.slot == strings.TrimPrefix(tc.slot, "pool ") {
				spec = s
			}
		}
		if d.UnraidRole != tc.unraid || d.ProposedRole != tc.proposed || d.DiskNumber != tc.number || d.Filesystem != tc.filesystem {
			t.Errorf("%s = %+v, want role %s proposed %s number %d filesystem %s", tc.slot, d, tc.unraid, tc.proposed, tc.number, tc.filesystem)
		}
		if d.Serial != spec.serial() || d.UnraidID != spec.id() || d.Size != spec.size || d.Device != e.dev[spec.slot] {
			t.Errorf("%s identity = %+v, want serial %s id %s size %d device %s", tc.slot, d, spec.serial(), spec.id(), spec.size, e.dev[spec.slot])
		}
		if d.WeakIdentity == nil || *d.WeakIdentity || d.Refused || d.RefusalCode != "" || d.Refusal != "" || d.Problem != "" {
			t.Errorf("%s = %+v, want a strong identity, not refused, with no problem", tc.slot, d)
		}
	}
	if len(r.Review.Shares) == 0 || r.Review.Boot.Mode != "usb" || r.Review.Capture.State != CapturePresent {
		t.Errorf("review = %+v", r.Review)
	}
}

// The Unraid role comes from disks.ini, and so does the pre-fill: without it
// nothing is proposed, and a disk the capture does not name is not called
// unassigned, because nothing says it was not a data disk.
func TestReview_WithoutDisksININothingIsProposedAndNoDiskIsCalledUnassigned(t *testing.T) {
	e := newDataEnv(t, primary)
	delete(e.files, "config/hoserva/disks.ini")
	r := e.scan()
	var pool *ReviewDisk
	for i, d := range r.Review.Disks {
		if d.ProposedRole != "" {
			t.Errorf("%+v is proposed a role with no disks.ini", d)
		}
		if d.UnraidRole == UnraidUnassigned {
			t.Errorf("%+v is called unassigned with no disks.ini", d)
		}
		if d.Slot == "pool cache" {
			pool = &r.Review.Disks[i]
		}
	}
	if pool == nil || pool.UnraidRole != UnraidCache {
		t.Errorf("the pool's role comes from its own config, not disks.ini: %+v", pool)
	}
	if len(r.Review.Disks) != 5 {
		t.Errorf("disks = %d, want the pool device and the four disks it cannot place, each once: %+v", len(r.Review.Disks), r.Review.Disks)
	}
}

// Each way the scan refuses a disk is a code beside the row's own prose, and a
// refused disk is never proposed the data role.
func TestReview_ARefusedDiskCarriesItsCodeAndTheRowsProse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  RefusalCode
		check string
		prep  func(e *dataEnv)
	}{
		{"zfs", RefuseZFS, CheckDataDisks, func(e *dataEnv) { e.setDisk("disk2", func(d *disk.Disk) { d.Filesystem = "zfs_member" }) }},
		{"encrypted", RefuseEncrypted, CheckDataDisks, func(e *dataEnv) { e.setDisk("disk2", func(d *disk.Disk) { d.Filesystem = "crypto_LUKS" }) }},
		{"other filesystem", RefuseUnsupportedFS, CheckDataDisks, func(e *dataEnv) { e.setDisk("disk2", func(d *disk.Disk) { d.Filesystem = "ntfs" }) }},
		{"no filesystem", RefuseNoFilesystem, CheckDataDisks, func(e *dataEnv) { e.setDisk("disk2", func(d *disk.Disk) { d.Filesystem = "" }) }},
		{"flash and device disagree", RefuseFilesystemClash, CheckDataDisks, func(e *dataEnv) { e.setDisk("disk2", func(d *disk.Disk) { d.Filesystem = "ext4" }) }},
		{"no filesystem uuid", RefuseNoFilesystemNode, CheckDataDisks, func(e *dataEnv) { e.setDisk("disk2", func(d *disk.Disk) { d.FSUUID = "" }) }},
		{"duplicate uuid", RefuseDuplicateUUID, CheckDataDisks, func(e *dataEnv) {
			list, _ := e.disks.List(context.Background())
			for _, o := range list {
				if o.Device == e.dev["disk3"] {
					e.setDisk("disk2", func(d *disk.Disk) { d.FSUUID = o.FSUUID })
				}
			}
		}},
		{"failed", RefuseFailed, CheckDataDisks, func(e *dataEnv) { e.setDisk("disk2", func(d *disk.Disk) { d.Failed = true }) }},
		{"this machine's boot disk", RefuseHostBoot, CheckDataDisks, func(e *dataEnv) { e.setDisk("disk2", func(d *disk.Disk) { d.Boot = true }) }},
		{"an Unraid boot device", RefuseBootDevice, CheckBootDevice, func(e *dataEnv) {
			e.setDisk("disk2", func(d *disk.Disk) { d.UnraidBoot = true; d.Filesystem = "vfat" })
		}},
		{"integrity check", RefuseIntegrity, CheckIntegrity, func(e *dataEnv) {
			e.runner.Script("xfs_repair", []string{"-n", e.dev["disk2"] + "1"}, nil, errors.New("exit status 1: a bad free-space B-tree block"))
		}},
		{"multi-device btrfs", RefuseMultiDeviceBtrfs, CheckDataDisks, func(e *dataEnv) {
			e.setFS("disk2", 2, "btrfs")
			e.scriptBtrfs(e.dev["disk2"]+"1", 2, 0)
		}},
		{"btrfs superblock unreadable", RefuseUnverifiedFS, CheckDataDisks, func(e *dataEnv) {
			e.setFS("disk2", 2, "btrfs")
			e.runner.Script("btrfs", []string{"inspect-internal", "dump-super", e.dev["disk2"] + "1"}, nil, errors.New("exit status 1"))
		}},
		{"btrfs tree log", RefusePendingLog, CheckIntegrity, func(e *dataEnv) {
			e.setFS("disk2", 2, "btrfs")
			e.scriptBtrfs(e.dev["disk2"]+"1", 1, 31391744)
		}},
		{"ext4 journal", RefusePendingLog, CheckIntegrity, func(e *dataEnv) {
			e.setFS("disk2", 2, "ext4")
			e.scriptExt4(e.dev["disk2"]+"1", "has_journal needs_recovery extent")
		}},
		{"ext4 journal unreadable", RefuseUnverifiedFS, CheckIntegrity, func(e *dataEnv) {
			e.setFS("disk2", 2, "ext4")
			e.runner.Script("dumpe2fs", []string{"-h", e.dev["disk2"] + "1"}, nil, errors.New("exit status 1"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDataEnv(t, primary)
			tc.prep(e)
			r := e.scan()
			d := reviewDisk(t, r, "disk2")
			row, ok := rowFor(r, tc.check, "disk2")
			if !ok || row.Status != StatusRefuse {
				t.Fatalf("no refuse row for disk2: %+v", rowsFor(r, tc.check))
			}
			if !d.Refused || d.RefusalCode != tc.code || d.Refusal != row.Detail {
				t.Errorf("disk2 = refused %v code %q refusal %q, want %q and the row's own text %q", d.Refused, d.RefusalCode, d.Refusal, tc.code, row.Detail)
			}
			if tc.code == RefuseBootDevice {
				if d.ProposedRole != ProposeIgnore {
					t.Errorf("a boot device is proposed %q, want ignore", d.ProposedRole)
				}
			} else if d.ProposedRole != "" {
				t.Errorf("a refused disk is proposed %q, want nothing", d.ProposedRole)
			}
			if d1 := reviewDisk(t, r, "disk1"); d1.Refused || d1.ProposedRole != ProposeData {
				t.Errorf("an unrefused disk = %+v", d1)
			}
		})
	}
}

// A disk that passed its checks and then could not be read completely is
// refused too, with its own code.
func TestReview_ADiskThatCannotBeReadCompletelyIsRefusedAsUnreadable(t *testing.T) {
	e := newDataEnv(t, primary)
	e.mounter.OnMount = func(string) error {
		if e.mounter.Mounts[len(e.mounter.Mounts)-1].Device == e.dev["disk2"]+"1" {
			return errors.New("input/output error")
		}
		return nil
	}
	r := e.scan()
	d := reviewDisk(t, r, "disk2")
	if !d.Refused || d.RefusalCode != RefuseUnreadable || d.ProposedRole != "" {
		t.Errorf("disk2 = %+v, want refused as unreadable", d)
	}
}

// The first refusal is the one the table keeps: a data slot whose disk is the
// capture's boot device is refused by the mapping check and again by the data
// disk checks, both as a boot device.
func TestReview_ASlotWhoseDiskIsTheCapturesBootDeviceIsRefusedOnceAsABootDevice(t *testing.T) {
	e := newDataEnv(t, primary)
	e.files["config/hoserva/capture.json"] = []byte(`{"unraid_version":"6.12.15","boot":{"mode":"internal","filesystem":"zfs","devices":[{"name":"boot","serial":"disk2-hoserva-test","model":"m","size":"1G"}]}}`)
	r := e.scan()
	d := reviewDisk(t, r, "disk2")
	if !d.Refused || d.RefusalCode != RefuseBootDevice || d.ProposedRole != ProposeIgnore {
		t.Errorf("disk2 = %+v", d)
	}
	n := 0
	for _, row := range r.Review.Disks {
		if row.Serial == "disk2-hoserva-test" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("disk2's device is in the table %d times, want once: %+v", n, r.Review.Disks)
	}
}

// A parity disk with only a weak identity is refused (Q21); a data disk with
// one is only flagged, and is still proposed the data role.
func TestReview_WeakIdentityRefusesParityAndOnlyFlagsData(t *testing.T) {
	e := newDataEnv(t, primary)
	e.setDisk("parity", func(d *disk.Disk) { d.WeakIdentity = true })
	e.setDisk("disk1", func(d *disk.Disk) { d.WeakIdentity = true })
	r := e.scan()

	p := reviewDisk(t, r, "parity")
	if p.WeakIdentity == nil || !*p.WeakIdentity || !p.Refused || p.RefusalCode != RefuseWeakIdentityParity || p.ProposedRole != "" {
		t.Errorf("parity = %+v", p)
	}
	if row := findRow(t, r, CheckIdentity, "parity"); p.Refusal != row.Detail {
		t.Errorf("parity refusal = %q, want the row's %q", p.Refusal, row.Detail)
	}
	d := reviewDisk(t, r, "disk1")
	if d.WeakIdentity == nil || !*d.WeakIdentity || d.Refused || d.ProposedRole != ProposeData {
		t.Errorf("disk1 = %+v, want weak, not refused and proposed data", d)
	}
	if d2 := reviewDisk(t, r, "disk2"); d2.WeakIdentity == nil || *d2.WeakIdentity {
		t.Errorf("a disk with a serial is not weak: %+v", d2)
	}
}

func disksWithout(spec fixtureSpec, slot string) *disk.FakeProvider {
	p := disk.NewFakeProvider()
	for i, d := range spec.disks {
		if d.slot == slot {
			continue
		}
		fsType := d.fs
		if d.kind == "parity" {
			fsType = "xfs"
		}
		p.AddDisk(fmt.Sprintf("/dev/sd%c", "bcdefghij"[i]), disk.Disk{Serial: d.serial(), Size: d.size, Filesystem: fsType})
	}
	return p
}

// A slot whose disk is not attached is a row with no identity of this machine:
// the size is what Unraid recorded, the identity is Unraid's own, and whether it
// is weak is unknown, not false.
func TestReview_ASlotWithNoDiskHereHasNoIdentityAndAnUnknownWeakness(t *testing.T) {
	files, spec := flashTree(t, primary)
	r, err := scanner(disksWithout(spec, "disk2")).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d := reviewDisk(t, r, "disk2")
	if d.Device != "" || d.Serial != "" || d.WeakIdentity != nil || d.HostBoot != nil || d.Filesystem != "" {
		t.Errorf("disk2 has an identity of this machine: %+v", d)
	}
	if d.UnraidID != "FIXTURE_disk2-hoserva-test" || d.Size != 320<<20 || d.Problem != "no disk on this machine has this serial or WWN" || d.UnraidRole != UnraidData || d.DiskNumber != 2 {
		t.Errorf("disk2 = %+v", d)
	}
	if d.Refused {
		t.Errorf("a disk that is not attached was refused: %+v", d)
	}
	if d.ProposedRole != "" {
		t.Errorf("disk2 has no disk here and is proposed %q: a slot with no matching disk proposes nothing", d.ProposedRole)
	}
	if d1 := reviewDisk(t, r, "disk1"); d1.ProposedRole != ProposeData {
		t.Errorf("disk1 = %+v, want a slot with its disk still proposed data", d1)
	}
}

// A disk of this machine the capture does not name is unassigned, once; this
// machine's own boot disk is not offered, and a disk that more than one slot or
// disk claims is not also listed as unassigned beside the slot that names it.
func TestReview_UnnamedDisksAreUnassignedAndNothingAppearsTwice(t *testing.T) {
	files, spec := flashTree(t, primary)
	p := fixtureDisks(spec)
	p.AddDisk("/dev/sdm", disk.Disk{Serial: "spare", Size: 1 << 30, Filesystem: "ext4"})
	p.AddDisk("/dev/sdn", disk.Disk{Serial: "this-machine", Size: 1 << 30, Filesystem: "ext4", Boot: true})
	p.AddDisk("/dev/sdp", disk.Disk{Serial: "disk2-hoserva-test", Size: 320 << 20, Filesystem: "xfs"})
	r, err := scanner(p).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var unassigned []string
	seen := map[string]int{}
	for _, d := range r.Review.Disks {
		if d.Device != "" {
			seen[d.Device]++
		}
		if d.UnraidRole == UnraidUnassigned {
			unassigned = append(unassigned, d.Device)
			if d.ProposedRole != "" || d.Slot != "" {
				t.Errorf("an unnamed disk has a slot or a proposed role: %+v", d)
			}
		}
	}
	if !reflect.DeepEqual(unassigned, []string{"/dev/sdm"}) {
		t.Errorf("unassigned = %v, want only the spare", unassigned)
	}
	for dev, n := range seen {
		if n != 1 {
			t.Errorf("%s is in the table %d times", dev, n)
		}
	}
	if seen["/dev/sdn"] != 0 {
		t.Errorf("this machine's boot disk is offered")
	}
	if d := reviewDisk(t, r, "disk2"); d.Device != "" || !strings.Contains(d.Problem, "2 disks on this machine match") {
		t.Errorf("disk2 = %+v, want no device and the ambiguity", d)
	}
}

func bootCapture(boot string) func(map[string][]byte) {
	return func(f map[string][]byte) {
		f["config/hoserva/capture.json"] = []byte(`{"unraid_version":"7.3.2","captured_at":"2026-10-03T07:02:18Z","boot":` + boot + `}`)
	}
}

func TestReview_BootModeAndLayout(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name   string
		boot   string
		mode   string
		mirror *bool
		shared *bool
	}{
		{"usb", `{"mode":"usb","devices":[],"mirrored":false,"shared_with_data_pool":false}`, "usb", nil, nil},
		{"internal on a device of its own", `{"mode":"internal","filesystem":"zfs","devices":[{"name":"nvme0n1","serial":"B1","model":"m","size":"500G"}],"mirrored":false,"shared_with_data_pool":false}`, "internal", &no, &no},
		{"internal sharing its disk with the cache", `{"mode":"internal","filesystem":"zfs","devices":[{"name":"nvme0n1","serial":"B1","model":"m","size":"500G"}],"mirrored":false,"shared_with_data_pool":true}`, "internal", &no, &yes},
		{"internal mirrored pair", `{"mode":"internal","filesystem":"zfs","devices":[{"name":"a","serial":"B1","model":"m","size":"500G"},{"name":"b","serial":"B2","model":"m","size":"500G"}],"mirrored":true,"shared_with_data_pool":false}`, "internal", &yes, &no},
		{"a mode that is neither", `{"mode":"floppy","devices":[]}`, "", nil, nil},
		{"no mode", `{"devices":[]}`, "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := scanInventory(t, bootCapture(tc.boot))
			got := r.Review.Boot
			if got.Mode != tc.mode || !reflect.DeepEqual(got.Mirrored, tc.mirror) || !reflect.DeepEqual(got.SharedWithCache, tc.shared) {
				t.Errorf("boot = %+v, want mode %q mirrored %v shared %v", got, tc.mode, deref(tc.mirror), deref(tc.shared))
			}
		})
	}
	t.Run("no capture", func(t *testing.T) {
		r := scanInventory(t, func(f map[string][]byte) { delete(f, "config/hoserva/capture.json") })
		if r.Review.Boot != (ReviewBoot{}) {
			t.Errorf("boot = %+v with no capture, want nothing known", r.Review.Boot)
		}
	})
}

func deref(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// An internal-boot device is a row of the table whose only role is ignore:
// matched to this machine's disk by serial when it is attached, named from the
// capture when it is not, and never in the table twice.
func TestReview_AnInternalBootDeviceIsARowThatCanOnlyBeIgnored(t *testing.T) {
	files, spec := flashTree(t, primary)
	bootCapture(`{"mode":"internal","filesystem":"zfs","devices":[{"name":"nvme0n1","serial":"B1","model":"Boot SSD","size":"500G"},{"name":"nvme1n1","serial":"B2","model":"Boot SSD","size":"500G"}],"mirrored":true,"shared_with_data_pool":false}`)(files)
	p := fixtureDisks(spec)
	p.AddDisk("/dev/nvme0n1", disk.Disk{Serial: "B1", Size: 500 << 30, Model: "Boot SSD", UnraidBoot: true, Filesystem: "zfs_member", WWN: "eui.0025"})
	r, err := scanner(p).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var boots []ReviewDisk
	for _, d := range r.Review.Disks {
		if d.UnraidRole == UnraidBoot {
			boots = append(boots, d)
		}
	}
	if len(boots) != 2 {
		t.Fatalf("boot rows = %+v, want one per device the capture names", boots)
	}
	for _, d := range boots {
		if d.ProposedRole != ProposeIgnore || d.Slot != "boot" || d.Refused {
			t.Errorf("boot row = %+v, want ignore", d)
		}
	}
	if b := boots[0]; b.Device != "/dev/nvme0n1" || b.WWN != "eui.0025" || b.Size != 500<<30 || b.WeakIdentity == nil {
		t.Errorf("the attached boot device = %+v, want this machine's disk", b)
	}
	if b := boots[1]; b.Device != "" || b.Serial != "B2" || b.Model != "Boot SSD" || b.Size != 0 || b.WeakIdentity != nil {
		t.Errorf("the boot device that is not attached = %+v, want only what the capture says", b)
	}
}

// The USB stick on this machine is a boot row as well, whether or not the
// capture names a device.
func TestReview_TheUnraidStickIsABootRow(t *testing.T) {
	files, spec := flashTree(t, primary)
	p := fixtureDisks(spec)
	p.AddDisk("/dev/sdx", disk.Disk{Serial: "STICK", Size: 8 << 30, Filesystem: disk.UnraidStickFilesystem, Label: disk.UnraidStickLabel})
	r, err := scanner(p).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d := reviewDisk(t, r, "boot")
	if d.Device != "/dev/sdx" || d.UnraidRole != UnraidBoot || d.ProposedRole != ProposeIgnore {
		t.Errorf("the stick = %+v", d)
	}
}

// The capture is present, missing, unreadable or stale, from the one decision
// the report's rows use.
func TestReview_CaptureState(t *testing.T) {
	const tmpl = "config/plugins/dockerMan/templates-user/my-gateway.xml"
	captured := committedCaptureTime(t)

	run := func(t *testing.T, c scanCase) *Report {
		r, _ := c.run(t)
		return r
	}
	t.Run("present", func(t *testing.T) {
		r := run(t, scanCase{times: map[string]time.Time{tmpl: captured.Add(-time.Hour)}})
		if got := r.Review.Capture; got.State != CapturePresent || got.CapturedAt == nil || !got.CapturedAt.Equal(captured) {
			t.Errorf("capture = %+v, want present at %v", got, captured)
		}
	})
	t.Run("stale", func(t *testing.T) {
		r := run(t, scanCase{times: map[string]time.Time{tmpl: captured.Add(time.Hour)}})
		if got := r.Review.Capture; got.State != CaptureStale || got.CapturedAt == nil || !got.CapturedAt.Equal(captured) {
			t.Errorf("capture = %+v, want stale since %v", got, captured)
		}
		if !hasRow(r, CheckCapture, StatusWarn, "may be stale") {
			t.Errorf("the stale row is gone: %+v", rowsFor(r, CheckCapture))
		}
	})
	t.Run("missing", func(t *testing.T) {
		r := run(t, scanCase{mutate: func(f map[string][]byte) { delete(f, "config/hoserva/capture.json") }})
		if got := r.Review.Capture; got.State != CaptureMissing || got.CapturedAt != nil {
			t.Errorf("capture = %+v, want missing", got)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		r := run(t, scanCase{mutate: func(f map[string][]byte) { f["config/hoserva/capture.json"] = []byte("{not json") }})
		if got := r.Review.Capture; got.State != CaptureUnreadable || got.CapturedAt != nil {
			t.Errorf("capture = %+v, want unreadable", got)
		}
	})
	t.Run("a time that is not a timestamp is not checked and not called fresh", func(t *testing.T) {
		r := run(t, scanCase{mutate: func(f map[string][]byte) {
			f["config/hoserva/capture.json"] = []byte(`{"unraid_version":"7.3.2","captured_at":"yesterday","boot":{"mode":"usb"}}`)
		}, times: map[string]time.Time{tmpl: captured.Add(time.Hour)}})
		if got := r.Review.Capture; got.State != CapturePresent || got.CapturedAt != nil {
			t.Errorf("capture = %+v, want present with no time", got)
		}
		if !hasRow(r, CheckCapture, StatusWarn, "not a timestamp") {
			t.Errorf("rows = %+v", rowsFor(r, CheckCapture))
		}
	})
	t.Run("the stick source decides the same way", func(t *testing.T) {
		files, _ := flashTree(t, inventoryVariant)
		dir := t.TempDir()
		if err := writeTree(dir, files); err != nil {
			t.Fatal(err)
		}
		src, err := OpenDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = src.Close() }()
		f, err := ReadFlash(src)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.ReviewCapture(); got.State != CapturePresent {
			t.Fatalf("capture = %+v, want present before a template is saved later", got)
		}
		later := captured.Add(time.Hour)
		if err := os.Chtimes(dir+"/"+tmpl, later, later); err != nil {
			t.Fatal(err)
		}
		f, err = ReadFlash(src)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.ReviewCapture(); got.State != CaptureStale {
			t.Errorf("capture = %+v, want stale", got)
		}
	})
}

// The share preview is built from the same config as the share's row: Unraid's
// method, High-water as a flag, the disks it is limited to, and as many
// warnings as the row flags.
func TestReview_TheSharePreviewMatchesTheSharesRows(t *testing.T) {
	r, _ := scanCase{mutate: func(f map[string][]byte) {
		f["config/shares/media.cfg"] = []byte("shareAllocator=\"highwater\"\nshareUseCache=\"no\"\nshareInclude=\"disk1,disk2\"\nshareExclude=\"disk3\"\n")
		f["config/shares/backup.cfg"] = []byte("shareAllocator=\"fillup\"\nshareUseCache=\"no\"\n")
		f["config/shares/documents.cfg"] = []byte("shareAllocator=\"roundrobin\"\nshareUseCache=\"sometimes\"\n")
	}}.run(t)

	byName := map[string]SharePreview{}
	for _, sh := range r.Review.Shares {
		byName[sh.Name] = sh
	}
	media := byName["media"]
	if !media.HighWater || media.AllocationMethod != "highwater" || !reflect.DeepEqual(media.Include, []string{"disk1", "disk2"}) || !reflect.DeepEqual(media.Exclude, []string{"disk3"}) || media.WarningCount != 1 {
		t.Errorf("media = %+v, want High-water, limited to disk1 and disk2, off disk3, one warning", media)
	}
	backup := byName["backup"]
	if backup.HighWater || backup.AllocationMethod != "fillup" || backup.WarningCount != 0 || backup.Include == nil || backup.Exclude == nil {
		t.Errorf("backup = %+v", backup)
	}
	docs := byName["documents"]
	if docs.HighWater || docs.AllocationMethod != "roundrobin" || docs.WarningCount != 2 {
		t.Errorf("documents = %+v, want a method and a cache setting Hoserva does not map, two warnings", docs)
	}

	rows := map[string]Row{}
	for _, row := range rowsFor(r, CheckShares) {
		rows[row.Subject] = row
	}
	for name, sh := range byName {
		row, ok := rows[name]
		if !ok {
			t.Errorf("%s is previewed and has no row", name)
			continue
		}
		if (sh.WarningCount > 0) != (row.Status == StatusWarn || row.Status == StatusFlag) {
			t.Errorf("%s: %d warnings but the row reads %s", name, sh.WarningCount, row.Status)
		}
	}
	for name, row := range rows {
		if _, ok := byName[name]; name != "" && !ok && !strings.Contains(row.Detail, "orphan") {
			t.Errorf("%s has a share row and no preview: %+v", name, row)
		}
	}
	if len(byName) != len(r.Import.Shares) {
		t.Errorf("%d previews for %d shares the import keeps", len(byName), len(r.Import.Shares))
	}
}

// A share with no directory on any matched disk is flagged by its row, and the
// preview counts that.
func TestReview_AShareWithNoDirectoryIsCountedAsWarned(t *testing.T) {
	e := newDataEnv(t, primary)
	r := e.scan()
	preview := func(name string) SharePreview {
		for _, sh := range r.Review.Shares {
			if sh.Name == name {
				return sh
			}
		}
		t.Fatalf("no preview for %s", name)
		return SharePreview{}
	}
	if row := findRow(t, r, CheckShares, "backup"); row.Status != StatusFlag || !strings.Contains(row.Detail, "no directory on any matched data disk") {
		t.Fatalf("row = %+v", row)
	}
	if got := preview("backup"); got.WarningCount != 1 {
		t.Errorf("backup = %+v, want its missing directory counted once", got)
	}
	if got := preview("media"); got.WarningCount != 1 || !got.HighWater {
		t.Errorf("media = %+v, has a directory on the disks, so only High-water's missing equivalent is a warning", got)
	}
	if got := preview("isos"); got.WarningCount != 2 || !got.HighWater {
		t.Errorf("isos = %+v, want High-water and its missing directory counted, two warnings", got)
	}
}

// The review survives the session's JSON round trip, and a report made before
// it existed reads back without one rather than with an empty one.
func TestReview_RoundTripsThroughTheSessionAndAnOldReportHasNone(t *testing.T) {
	r := scanInventory(t, nil)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Review, r.Review) {
		t.Errorf("review after a round trip:\n%+v\nwant\n%+v", back.Review, r.Review)
	}

	var old Report
	if err := json.Unmarshal([]byte(`{"generatedAt":"2026-10-03T00:00:00Z","unraidVersion":"7.3.2","unverifiedLayout":false,"verdict":"go","rows":[]}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Review != nil {
		t.Errorf("a report made before the review has %+v", old.Review)
	}
}

func cacheSerial(e *dataEnv) string {
	for _, d := range e.spec.disks {
		if d.slot == "cache" {
			return d.serial()
		}
	}
	e.t.Fatal("no cache disk in the fixture")
	return ""
}

// An internal boot that shares its disk with the cache is one row, the pool's:
// it is proposed cache (doc 05 §4 keeps the data partition as the cache) and
// says it is also the Unraid boot device, and no second boot row names the same
// device with the role ignore.
func TestReview_ASharedInternalBootDeviceIsOneRowTheCachePoolsMarkedAsBoot(t *testing.T) {
	e := newDataEnv(t, primary)
	e.files["config/hoserva/capture.json"] = []byte(`{"unraid_version":"7.3.2","boot":{"mode":"internal","filesystem":"zfs","devices":[{"name":"nvme0n1","serial":"` + cacheSerial(e) + `","model":"m","size":"1G"}],"mirrored":false,"shared_with_data_pool":true}}`)
	r := e.scan()
	var rows []ReviewDisk
	for _, d := range r.Review.Disks {
		if d.Device == e.dev["cache"] {
			rows = append(rows, d)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("the shared device is %d rows, want one: %+v", len(rows), rows)
	}
	if d := rows[0]; d.Slot != "pool cache" || d.UnraidRole != UnraidCache || d.ProposedRole != ProposeCache || !d.UnraidBoot || d.Refused {
		t.Errorf("the shared device = %+v, want the cache pool's row, proposed cache, marked as the Unraid boot device", d)
	}
	if r.Review.Boot.SharedWithCache == nil || !*r.Review.Boot.SharedWithCache {
		t.Errorf("boot = %+v, want shared with the cache", r.Review.Boot)
	}
	for _, d := range r.Review.Disks {
		if d.Slot != "pool cache" && d.UnraidBoot {
			t.Errorf("%+v is marked as the Unraid boot device beside the pool's row", d)
		}
	}
}

// A parity or data disk of this machine that holds the operating system running
// now is never proposed a role, whichever slot Unraid had it in: a parity disk
// Debian was installed on is refused like a data disk, with the row saying so.
func TestReview_ThisMachinesBootDiskInAParityOrDataSlotIsNeverProposedARole(t *testing.T) {
	for _, slot := range []string{"parity", "disk1"} {
		t.Run(slot, func(t *testing.T) {
			e := newDataEnv(t, primary)
			e.setDisk(slot, func(d *disk.Disk) { d.Boot = true })
			r := e.scan()
			d := reviewDisk(t, r, slot)
			if !d.Refused || d.RefusalCode != RefuseHostBoot || d.ProposedRole != "" {
				t.Errorf("%s = %+v, want refused as host_boot and proposed nothing", slot, d)
			}
			var said bool
			for _, row := range r.Rows {
				if row.Subject == slot && row.Status == StatusRefuse && row.Detail == d.Refusal {
					said = true
				}
			}
			if !said {
				t.Errorf("no refuse row says %q", d.Refusal)
			}
			if r.Verdict != VerdictNoGo {
				t.Errorf("verdict = %s, want no_go with the boot disk in a slot", r.Verdict)
			}
		})
	}
}

// The shared NVMe (doc 01 §6, doc 05 §4 step 12): Unraid's cache was on the disk
// Debian is now installed on, so the cache pool names this machine's boot disk.
// That layout is supported: the pool row is not refused, the mapping row says
// what it said before the review existed, the verdict is not no-go, and the
// import pre-fills cache.
func TestReview_ThisMachinesBootDiskInTheCachePoolIsASharedNVMeAndProposedCache(t *testing.T) {
	e := newDataEnv(t, primary)
	e.setDisk("cache", func(d *disk.Disk) { d.Boot = true })
	r := e.scan()
	d := reviewDisk(t, r, "pool cache")
	if d.Refused || d.ProposedRole != ProposeCache {
		t.Errorf("pool cache = %+v, want not refused and proposed cache", d)
	}
	if r.Verdict == VerdictNoGo {
		t.Errorf("verdict = %s, want a shared NVMe cache not to be no_go", r.Verdict)
	}
	var rows []Row
	for _, row := range r.Rows {
		if row.Check == CheckMapping && row.Subject == "pool cache" {
			rows = append(rows, row)
		}
	}
	want := fmt.Sprintf("serial %s is a pool device; this machine has it as %s, %s.", d.UnraidID, d.Device, formatBytes(d.Size))
	if len(rows) != 1 || rows[0].Status != StatusPass || rows[0].Detail != want {
		t.Errorf("mapping rows for the cache pool = %+v, want one pass row %q", rows, want)
	}
}

// hostBoot is the scan's own boot-disk detection on the row it belongs to: true
// on the disk this machine boots from, false on every other disk of this machine
// the table lists (a boot row and an unassigned disk included), and unknown, not
// false, where no disk of this machine matched the row.
func TestReview_HostBootIsSetOnlyOnTheRowOfTheDiskThisMachineBootsFrom(t *testing.T) {
	hostBootRows := func(r *Report) []string {
		var out []string
		for _, d := range r.Review.Disks {
			if d.HostBoot != nil && *d.HostBoot {
				out = append(out, d.Slot)
			}
		}
		return out
	}
	assertOthersKnown := func(t *testing.T, r *Report) {
		t.Helper()
		for _, d := range r.Review.Disks {
			if (d.Device != "") != (d.HostBoot != nil) {
				t.Errorf("%+v: hostBoot is known exactly when a disk of this machine matched the row", d)
			}
		}
	}

	t.Run("no disk is the boot disk", func(t *testing.T) {
		files, spec := flashTree(t, primary)
		p := disksWithout(spec, "disk2")
		p.AddDisk("/dev/sdm", disk.Disk{Serial: "spare", Size: 1 << 30, Filesystem: "ext4"})
		p.AddDisk("/dev/sdx", disk.Disk{Serial: "STICK", Size: 8 << 30, Filesystem: disk.UnraidStickFilesystem, Label: disk.UnraidStickLabel})
		r, err := scanner(p).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := hostBootRows(r); len(got) != 0 {
			t.Errorf("hostBoot is true on %v, want no row", got)
		}
		assertOthersKnown(t, r)
		var unassigned, stick bool
		for _, d := range r.Review.Disks {
			unassigned = unassigned || d.UnraidRole == UnraidUnassigned && d.HostBoot != nil
			stick = stick || d.UnraidRole == UnraidBoot && d.HostBoot != nil
		}
		if !unassigned || !stick {
			t.Errorf("the unassigned disk and the stick have a known hostBoot: %v %v in %+v", unassigned, stick, r.Review.Disks)
		}
	})
	for _, slot := range []string{"parity", "disk1", "cache"} {
		t.Run(slot+" is the boot disk", func(t *testing.T) {
			e := newDataEnv(t, primary)
			e.setDisk(slot, func(d *disk.Disk) { d.Boot = true })
			r := e.scan()
			want := slot
			if slot == "cache" {
				want = "pool cache"
			}
			if got := hostBootRows(r); !reflect.DeepEqual(got, []string{want}) {
				t.Errorf("hostBoot is true on %v, want only %s", got, want)
			}
			assertOthersKnown(t, r)
		})
	}
	t.Run("an Unraid boot device that is the boot disk", func(t *testing.T) {
		files, spec := flashTree(t, primary)
		bootCapture(`{"mode":"internal","filesystem":"zfs","devices":[{"name":"nvme0n1","serial":"B1","model":"Boot SSD","size":"500G"}],"mirrored":false,"shared_with_data_pool":false}`)(files)
		p := fixtureDisks(spec)
		p.AddDisk("/dev/nvme0n1", disk.Disk{Serial: "B1", Size: 500 << 30, Model: "Boot SSD", UnraidBoot: true, Boot: true, Filesystem: "zfs_member"})
		r, err := scanner(p).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := hostBootRows(r); !reflect.DeepEqual(got, []string{"boot"}) {
			t.Errorf("hostBoot is true on %v, want only the boot row", got)
		}
	})
}

// unraidBoot is set on the row of any disk that is also an Unraid boot device,
// the refused parity and data rows as much as the shared cache's, and only on
// those: the proposed role is what the import reads, and it stays ignore there.
func TestReview_UnraidBootIsSetOnEverySlotRowOfAnUnraidBootDevice(t *testing.T) {
	e := newDataEnv(t, primary)
	e.setDisk("parity", func(d *disk.Disk) { d.UnraidBoot = true })
	e.setDisk("cache", func(d *disk.Disk) { d.UnraidBoot = true })
	r := e.scan()
	for slot, role := range map[string]ProposedRole{"parity": ProposeIgnore, "pool cache": ProposeCache} {
		if d := reviewDisk(t, r, slot); !d.UnraidBoot || d.ProposedRole != role {
			t.Errorf("%s = %+v, want unraidBoot and proposed %s", slot, d, role)
		}
	}
	for _, d := range r.Review.Disks {
		if d.Slot != "parity" && d.Slot != "pool cache" && d.UnraidBoot {
			t.Errorf("%+v is marked as an Unraid boot device, which no slot there is", d)
		}
	}
}

// An Unraid boot device or stick in a parity slot, which the capture does not
// name, is only ever ignored. The cache slot may keep an internal boot's data
// partition (doc 05 §4), so the same device there is still the cache.
func TestReview_AnUnraidBootDeviceInAParitySlotIsOnlyIgnored(t *testing.T) {
	for name, change := range map[string]func(*disk.Disk){
		"internal boot device": func(d *disk.Disk) { d.UnraidBoot = true },
		"stick":                func(d *disk.Disk) { d.Filesystem = disk.UnraidStickFilesystem; d.Label = disk.UnraidStickLabel },
	} {
		t.Run(name, func(t *testing.T) {
			e := newDataEnv(t, primary)
			e.setDisk("parity", change)
			e.setDisk("cache", change)
			r := e.scan()
			p := reviewDisk(t, r, "parity")
			if !p.Refused || p.RefusalCode != RefuseBootDevice || p.ProposedRole != ProposeIgnore {
				t.Errorf("parity = %+v, want refused as boot_device and only ignored", p)
			}
			if c := reviewDisk(t, r, "pool cache"); c.Refused || c.ProposedRole != ProposeCache {
				t.Errorf("pool cache = %+v, want the cache kept", c)
			}
		})
	}
}

// Without the data disk checks (a daemon not set up to read them) a data slot on
// this machine's boot disk or on an Unraid boot device still gets no data role.
func TestReview_ABootDiskInADataSlotIsNotProposedDataWhenTheDisksAreNotChecked(t *testing.T) {
	files, spec := flashTree(t, primary)
	p := fixtureDisks(spec)
	list, _ := p.List(context.Background())
	for _, d := range list {
		switch d.Serial {
		case "disk1-hoserva-test":
			d.Boot = true
		case "disk2-hoserva-test":
			d.UnraidBoot = true
		default:
			continue
		}
		p.AddDisk(d.Device, d)
	}
	r, err := scanner(p).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d := reviewDisk(t, r, "disk1"); d.ProposedRole != "" {
		t.Errorf("disk1 (this machine's boot disk) = %+v, want no role", d)
	}
	if d := reviewDisk(t, r, "disk2"); d.ProposedRole != ProposeIgnore {
		t.Errorf("disk2 (an Unraid boot device) = %+v, want ignore", d)
	}
	if d := reviewDisk(t, r, "disk3"); d.ProposedRole != ProposeData {
		t.Errorf("disk3 = %+v, want data", d)
	}
}

// proposedRoles is what accepting the scan's proposal sends to the import: every
// row that is proposed a role and matched a disk of this machine, by its WWN or
// else its serial.
func proposedRoles(r *Report) []disk.AdoptionAssignment {
	var out []disk.AdoptionAssignment
	for _, d := range r.Review.Disks {
		if d.ProposedRole == "" || d.Device == "" {
			continue
		}
		a := disk.AdoptionAssignment{Role: disk.AdoptionRole(d.ProposedRole)}
		if d.WWN != "" {
			a.WWN = d.WWN
		} else {
			a.Serial = d.Serial
		}
		out = append(out, a)
	}
	return out
}

func assertProposalImports(t *testing.T, r *Report, p disk.Provider) {
	t.Helper()
	listed, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PlanFromReview(r.Review, listed, proposedRoles(r)); err != nil {
		t.Errorf("the import refuses the scan's own proposal %+v: %v", proposedRoles(r), err)
	}
}

// importableDisks is this machine as the import sees it: fixtureDisks, with the
// filesystem node and UUID a data disk is adopted from.
func importableDisks(spec fixtureSpec) *disk.FakeProvider {
	p := disk.NewFakeProvider()
	for i, d := range spec.disks {
		fsType := d.fs
		if d.kind == "parity" {
			fsType = "xfs"
		}
		dev := fmt.Sprintf("/dev/sd%c", "bcdefghij"[i])
		p.AddDisk(dev, disk.Disk{
			Serial: d.serial(), Size: d.size, Filesystem: fsType, FSDevice: dev + "1",
			FSUUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1),
		})
	}
	return p
}

func proposedRole(t *testing.T, r *Report, slot string) ProposedRole {
	t.Helper()
	return reviewDisk(t, r, slot).ProposedRole
}

// What the scan proposes is what the import accepts: every fixture variant's
// proposal passes the import's own role validation unchanged.
func TestReview_TheProposedRolesAreRolesTheImportAccepts(t *testing.T) {
	for _, variant := range fixtureVariants {
		t.Run(variant, func(t *testing.T) {
			files, spec := flashTree(t, variant)
			p := importableDisks(spec)
			r, err := scanner(p).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(proposedRoles(r)) == 0 {
				t.Fatal("nothing is proposed, so nothing was checked")
			}
			assertProposalImports(t, r, p)
		})
	}
}

// Unraid's named pools: one disk is Hoserva's cache, from the pool named cache
// (doc 05 §2); every other pool's disk is proposed ignore, which leaves it
// untouched.
func TestReview_NamedPoolsProposeOneCacheDisk(t *testing.T) {
	t.Run("the pool named cache", func(t *testing.T) {
		files, spec := flashTree(t, "unraid-named-pools")
		p := importableDisks(spec)
		r, err := scanner(p).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := proposedRole(t, r, "pool cache"); got != ProposeCache {
			t.Errorf("pool cache = %q, want cache", got)
		}
		if got := proposedRole(t, r, "pool fast"); got != ProposeIgnore {
			t.Errorf("pool fast = %q, want ignore", got)
		}
		assertProposalImports(t, r, p)
	})
	t.Run("the pool named cache wins over one sorting before it", func(t *testing.T) {
		files, spec := flashTree(t, "unraid-named-pools")
		files["config/pools/archive.cfg"] = files["config/pools/fast.cfg"]
		delete(files, "config/pools/fast.cfg")
		p := importableDisks(spec)
		r, err := scanner(p).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if c, a := proposedRole(t, r, "pool cache"), proposedRole(t, r, "pool archive"); c != ProposeCache || a != ProposeIgnore {
			t.Errorf("cache = %q, archive = %q, want cache and ignore", c, a)
		}
		assertProposalImports(t, r, p)
	})
	t.Run("no pool named cache: the first by name", func(t *testing.T) {
		files, spec := flashTree(t, "unraid-named-pools")
		files["config/pools/archive.cfg"] = files["config/pools/cache.cfg"]
		delete(files, "config/pools/cache.cfg")
		p := importableDisks(spec)
		r, err := scanner(p).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if a, f := proposedRole(t, r, "pool archive"), proposedRole(t, r, "pool fast"); a != ProposeCache || f != ProposeIgnore {
			t.Errorf("archive = %q, fast = %q, want cache and ignore", a, f)
		}
		assertProposalImports(t, r, p)
	})
	t.Run("a pool of two disks", func(t *testing.T) {
		files, spec := flashTree(t, "unraid-named-pools")
		files["config/pools/cache.cfg"] = append(files["config/pools/cache.cfg"], []byte("diskId.1=\"FIXTURE_fast-hoserva-test\"\n")...)
		delete(files, "config/pools/fast.cfg")
		p := importableDisks(spec)
		r, err := scanner(p).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cache := 0
		for _, d := range r.Review.Disks {
			if d.ProposedRole == ProposeCache {
				cache++
			}
		}
		if cache != 1 {
			t.Errorf("%d disks are proposed cache, want one: %+v", cache, r.Review.Disks)
		}
		assertProposalImports(t, r, p)
	})
}

// An Unraid internal boot device with no cache on it is the boot pool's own
// device: the import refuses any role but ignore for it, so that is what is
// proposed, while another pool's disk is still proposed cache.
func TestReview_ADedicatedInternalBootDeviceIsProposedIgnore(t *testing.T) {
	build := func(t *testing.T, withCache bool) (*Report, disk.Provider) {
		files, spec := flashTree(t, "unraid-internal-boot")
		if withCache {
			spec.disks = append(spec.disks, fixtureDisk{slot: "fast", kind: "pool", fs: "xfs", size: 320 << 20})
			files["config/pools/fast.cfg"] = []byte("diskId=\"" + spec.disks[len(spec.disks)-1].id() + "\"\n")
		}
		p := importableDisks(spec)
		for i, d := range spec.disks {
			if d.kind == "boot" {
				dev := fmt.Sprintf("/dev/sd%c", "bcdefghij"[i])
				p.AddDisk(dev, disk.Disk{Serial: d.serial(), Size: d.size, Filesystem: "zfs_member", UnraidBoot: true})
			}
		}
		r, err := scanner(p).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return r, p
	}

	t.Run("no cache anywhere", func(t *testing.T) {
		r, p := build(t, false)
		d := reviewDisk(t, r, "pool boot")
		if !d.UnraidBoot || d.ProposedRole != ProposeIgnore {
			t.Errorf("the boot pool's row = %+v, want the Unraid boot device, proposed ignore", d)
		}
		assertProposalImports(t, r, p)
	})
	t.Run("another pool is the cache", func(t *testing.T) {
		r, p := build(t, true)
		if a, b := proposedRole(t, r, "pool boot"), proposedRole(t, r, "pool fast"); a != ProposeIgnore || b != ProposeCache {
			t.Errorf("boot = %q, fast = %q, want ignore and cache", a, b)
		}
		assertProposalImports(t, r, p)
	})
}
