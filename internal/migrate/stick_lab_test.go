//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on the
// host: it is built with `go test -tags lab -c` from the host (compiling touches
// no device) and the binary is run inside the lab container. It reads Unraid's
// configuration from a FAT image carrying a fixture variant's flash tree,
// attached as one of the lab's own loop devices, and shows the device is not
// written: its sha256 is the same before, during and after a full scan.
//
// The lab image has no mkfs.fat, so the image is formatted here, by writing the
// FAT32 structures the kernel's driver reads directly into the image file.

package migrate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

const (
	labStickUUID  = "5A17-C0DE"
	labStickBytes = 40 << 20
)

func labDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	return filepath.Join("/lab", id)
}

// formatFAT32 writes an empty FAT32 filesystem labelled UNRAID with volume id
// 0x5A17C0DE into img: one sector per cluster, the smallest cluster count a
// FAT32 volume may have (65525) or more, and a root directory in cluster 2.
func formatFAT32(t *testing.T, img string, size int64) {
	t.Helper()
	const (
		sector   = 512
		reserved = 32
		fats     = 2
	)
	total := uint32(size / sector)
	fatSectors := (total - reserved + (256+fats)/2 - 1) / ((256 + fats) / 2)
	if clusters := total - reserved - fats*fatSectors; clusters < 65525 {
		t.Fatalf("a %d byte FAT32 volume has %d clusters, below the 65525 a FAT32 volume needs", size, clusters)
	}

	boot := make([]byte, sector)
	copy(boot[0:], []byte{0xEB, 0x58, 0x90})
	copy(boot[3:], "MSWIN4.1")
	binary.LittleEndian.PutUint16(boot[11:], sector)
	boot[13] = 1
	binary.LittleEndian.PutUint16(boot[14:], reserved)
	boot[16] = fats
	boot[21] = 0xF8
	binary.LittleEndian.PutUint16(boot[24:], 32)
	binary.LittleEndian.PutUint16(boot[26:], 64)
	binary.LittleEndian.PutUint32(boot[32:], total)
	binary.LittleEndian.PutUint32(boot[36:], fatSectors)
	binary.LittleEndian.PutUint32(boot[44:], 2)
	binary.LittleEndian.PutUint16(boot[48:], 1)
	binary.LittleEndian.PutUint16(boot[50:], 6)
	boot[64] = 0x80
	boot[66] = 0x29
	binary.LittleEndian.PutUint32(boot[67:], 0x5A17C0DE)
	copy(boot[71:], "UNRAID     ")
	copy(boot[82:], "FAT32   ")
	boot[510], boot[511] = 0x55, 0xAA

	fsinfo := make([]byte, sector)
	binary.LittleEndian.PutUint32(fsinfo[0:], 0x41615252)
	binary.LittleEndian.PutUint32(fsinfo[484:], 0x61417272)
	binary.LittleEndian.PutUint32(fsinfo[488:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(fsinfo[492:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(fsinfo[508:], 0xAA550000)

	fat := make([]byte, sector)
	binary.LittleEndian.PutUint32(fat[0:], 0x0FFFFFF8)
	binary.LittleEndian.PutUint32(fat[4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(fat[8:], 0x0FFFFFFF)

	f, err := os.OpenFile(img, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	write := func(sectorNo uint32, b []byte) {
		if _, err := f.WriteAt(b, int64(sectorNo)*sector); err != nil {
			t.Fatal(err)
		}
	}
	write(0, boot)
	write(1, fsinfo)
	write(6, boot)
	write(7, fsinfo)
	write(reserved, fat)
	write(reserved+fatSectors, fat)
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

// attachLoop attaches img as a loop device and detaches it when the test ends,
// only while it still backs img: loop numbers are host-global, and another
// lab may have taken the number by then (CLAUDE.md).
func attachLoop(ctx context.Context, t *testing.T, r disk.Runner, img string) string {
	t.Helper()
	out, err := r.Run(ctx, "losetup", "--find", "--show", img)
	if err != nil {
		t.Fatalf("losetup --find --show %s: %v", img, err)
	}
	dev := strings.TrimSpace(string(out))
	if !strings.HasPrefix(dev, "/dev/loop") {
		t.Fatalf("losetup %s: got %q, want a /dev/loopN device", img, dev)
	}
	backing, err := r.Run(ctx, "losetup", "-j", img, "--output", "NAME", "--noheadings")
	if err != nil || strings.TrimSpace(string(backing)) != dev {
		t.Fatalf("losetup -j %s: got %q (err %v), want %q — refusing to trust a device this call did not just attach", img, backing, err, dev)
	}
	t.Cleanup(func() {
		raw, err := os.ReadFile(filepath.Join("/sys/block", filepath.Base(dev), "loop", "backing_file"))
		if err != nil || strings.TrimSuffix(strings.TrimSpace(string(raw)), " (deleted)") != img {
			t.Logf("not detaching %s: it no longer backs %s", dev, img)
			return
		}
		_, _ = r.Run(context.Background(), "losetup", "-d", dev)
	})
	return dev
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// mountOptionsAt returns the mount and superblock options the kernel lists for
// where, or "" when it is not mounted.
func mountOptionsAt(t *testing.T, where string) (mount, super string) {
	t.Helper()
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[4] != where {
			continue
		}
		mount = fields[5]
		for i, fl := range fields {
			if fl == "-" && len(fields) > i+3 {
				super = fields[i+3]
			}
		}
	}
	return mount, super
}

func hasOpt(options, want string) bool {
	for _, o := range strings.Split(options, ",") {
		if o == want {
			return true
		}
	}
	return false
}

// probeMounter is the real read-only mounter with a probe on each mount: it
// hashes the whole device and reads the mount options right after the mount and
// right before the unmount, because a read-write mount that is cleanly
// unmounted again puts FAT's dirty bit back and would leave no trace afterwards.
type probeMounter struct {
	disk.ReadOnlyMounter
	t       *testing.T
	device  string
	hashes  []string
	options [][2]string
	names   [][]string
}

func (p *probeMounter) probe(where string) {
	p.hashes = append(p.hashes, sha256File(p.t, p.device))
	src, err := OpenDir(where)
	if err != nil {
		p.t.Fatal(err)
	}
	p.names = append(p.names, src.List(""))
	_ = src.Close()
	m, s := mountOptionsAt(p.t, where)
	p.options = append(p.options, [2]string{m, s})
}

func (p *probeMounter) MountReadOnly(ctx context.Context, fsType, uuid, where string) error {
	if err := p.ReadOnlyMounter.MountReadOnly(ctx, fsType, uuid, where); err != nil {
		return err
	}
	p.probe(where)
	return nil
}

func (p *probeMounter) Unmount(ctx context.Context, where string) error {
	if mounted, _ := p.ReadOnlyMounter.IsMounted(ctx, where); mounted {
		p.probe(where)
	}
	return p.ReadOnlyMounter.Unmount(ctx, where)
}

// TestLabStick_ScanNeverWritesTheStick is the data-loss scenario of #297: the
// Unraid stick is the user's rollback, and a scan that writes to it, even FAT's
// dirty bit, has changed what the user boots from.
func TestLabStick_ScanNeverWritesTheStick(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := disk.CommandRunner{}

	// The fixture definitions are read relative to the package directory, and
	// the lab runs the binary from elsewhere, with the workspace at /src.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("/src/internal/migrate"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	imgDir := filepath.Join(lab, "img")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(imgDir, "unraid-stick-297.img")
	formatFAT32(t, img, labStickBytes)
	t.Cleanup(func() { _ = os.Remove(img) })
	dev := attachLoop(ctx, t, r, img)

	files, spec := flashTree(t, primary)
	files["config/plugins/dockerMan/templates-user/my-Fotos-é.xml"] = []byte("<Container/>\n")
	fill := filepath.Join(lab, "stick-fill")
	if err := os.MkdirAll(fill, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(fill) })
	if _, err := r.Run(ctx, "mount", "-t", "vfat", "-o", "utf8=1", dev, fill); err != nil {
		t.Fatalf("mounting the new image to fill it: %v", err)
	}
	filled := false
	t.Cleanup(func() {
		if !filled {
			_, _ = r.Run(context.Background(), "umount", fill)
		}
	})
	if err := writeTree(fill, files); err != nil {
		t.Fatalf("writing the flash tree to the image: %v", err)
	}
	if _, err := r.Run(ctx, "umount", fill); err != nil {
		t.Fatalf("unmounting the filled image: %v", err)
	}
	filled = true

	before := sha256File(t, dev)

	disks := fixtureDisks(spec)
	disks.AddDisk(dev, disk.Disk{Size: labStickBytes, Filesystem: "vfat", Label: "UNRAID", FSUUID: labStickUUID})
	svc := &Service{
		Dir:      filepath.Join(lab, "stick-scan", "migrate"),
		Scanner:  &Scanner{Disks: disks, UIDOwner: noUID, Now: fixedNow},
		Sessions: newSessions(t),
	}
	probe := &probeMounter{ReadOnlyMounter: disk.KernelReadOnlyMounter{Runner: r}, t: t, device: dev}
	svc.Mounter = probe
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(lab, "stick-scan")) })

	var scan string
	if err := svc.StartDeviceScan(ctx, dev, ScanOptions{}, func(_ context.Context, got string) (string, error) {
		scan = got
		return "job-1", nil
	}); err != nil {
		t.Fatalf("StartDeviceScan: %v", err)
	}
	if err := svc.RunScan(ctx, io.Discard, scan); err != nil {
		t.Fatalf("RunScan: %v", err)
	}

	if len(probe.hashes) != 4 {
		t.Fatalf("probed %d times, want a mount and an unmount for each of the two reads", len(probe.hashes))
	}
	for i, h := range probe.hashes {
		if h != before {
			t.Errorf("probe %d: the device's sha256 is %s, was %s before the scan: the stick was written while it was mounted", i, h, before)
		}
		if o := probe.options[i]; !hasOpt(o[0], "ro") || !hasOpt(o[1], "ro") {
			t.Errorf("probe %d: the stick was mounted %q (superblock %q), want read-only on both", i, o[0], o[1])
		}
	}
	zsrc := openZipBytes(t, zipOf(t, files, true))
	for i, names := range probe.names {
		if want := zsrc.List(""); !reflect.DeepEqual(names, want) {
			t.Errorf("probe %d: the mounted FAT lists\n%v\nthe zip of the same tree lists\n%v", i, names, want)
		}
	}
	if after := sha256File(t, dev); after != before {
		t.Errorf("after the scan the device's sha256 is %s, was %s", after, before)
	}
	where := filepath.Join(svc.Dir, "stick")
	if m, _ := mountOptionsAt(t, where); m != "" {
		t.Errorf("the stick is still mounted at %s (%s)", where, m)
	}

	// The result from the stick is the result from the same variant's zip.
	fromStick, err := svc.State(ctx)
	if err != nil || fromStick.Phase != PhaseScanned || fromStick.Report == nil {
		t.Fatalf("State = %+v, %v", fromStick, err)
	}
	zipSvc, _, _ := newService(t, primary)
	zipSvc.Scanner = &Scanner{Disks: fixtureDisks(spec), UIDOwner: noUID, Now: fixedNow}
	if err := scanNow(zipSvc, zipOf(t, files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	fromZip, _ := zipSvc.State(ctx)
	if !reflect.DeepEqual(fromStick.Report, fromZip.Report) {
		t.Errorf("the report read from the FAT stick differs from the zip's:\nstick %+v\nzip   %+v", fromStick.Report, fromZip.Report)
	}

	// The control arm: the same harness has to see what a read-write mount
	// does to the device, or the assertions above prove nothing. FAT sets its
	// dirty bit in the boot sector as soon as it is mounted read-write, with
	// nothing else done to it.
	rw := filepath.Join(lab, "stick-rw")
	if err := os.MkdirAll(rw, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(rw) })
	if _, err := r.Run(ctx, "mount", "-t", "vfat", "-o", "rw", "-U", labStickUUID, rw); err != nil {
		t.Fatalf("control: mounting read-write: %v", err)
	}
	t.Cleanup(func() { _, _ = r.Run(context.Background(), "umount", rw) })
	if got := sha256File(t, dev); got == before {
		t.Errorf("control: a read-write mount with nothing done left the device's sha256 unchanged (%s): this harness would not have seen the stick being written", got)
	}
	if m, _ := mountOptionsAt(t, rw); hasOpt(m, "ro") {
		t.Errorf("control: the read-write mount is listed %q", m)
	}
}
