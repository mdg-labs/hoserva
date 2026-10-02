package disk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const linuxDataGUID = "0fc63daf-8483-4772-8e79-3d69d8477de4"

type partFixture struct {
	num       int
	sectors   int64
	partType  string
	partUUID  string
	partName  string
	fsType    string
	holders   []string
	noByID    bool
	noUdev    bool
	noHolders bool
}

// newBootNVMeLister builds a synthetic tree for one NVMe that holds the
// root filesystem, plus the partitions in parts, and an SATA data disk.
// Every source the candidate check reads is a file under t.TempDir().
func newBootNVMeLister(t *testing.T, parts []partFixture, swaps, fstab string, units map[string]string) *Lister {
	t.Helper()
	root := t.TempDir()
	sysBlock := filepath.Join(root, "sys-class-block")
	byID := filepath.Join(root, "by-id")
	udev := filepath.Join(root, "udev")
	unitDir := filepath.Join(root, "units")
	mustMkdirAll(t, sysBlock)
	mustMkdirAll(t, byID)
	mustMkdirAll(t, udev)
	mustMkdirAll(t, unitDir)

	const parent = "nvme0n1"
	mustMkdirAll(t, filepath.Join(sysBlock, parent, "device"))
	mustWriteFile(t, filepath.Join(sysBlock, parent, "size"), "1953525168\n")
	mustWriteFile(t, filepath.Join(sysBlock, parent, "device", "model"), "Samsung SSD 970 EVO Plus 1TB\n")
	mustSymlink(t, "../../"+parent, filepath.Join(byID, "nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456X"))

	for _, p := range parts {
		name := fmt.Sprintf("%sp%d", parent, p.num)
		mustMkdirAll(t, filepath.Join(sysBlock, name))
		mustWriteFile(t, filepath.Join(sysBlock, name, "size"), fmt.Sprintf("%d\n", p.sectors))
		mustWriteFile(t, filepath.Join(sysBlock, name, "partition"), fmt.Sprintf("%d\n", p.num))
		mustWriteFile(t, filepath.Join(sysBlock, name, "dev"), fmt.Sprintf("259:%d\n", p.num))
		if !p.noHolders {
			mustMkdirAll(t, filepath.Join(sysBlock, name, "holders"))
			for _, h := range p.holders {
				mustMkdirAll(t, filepath.Join(sysBlock, name, "holders", h))
			}
		}
		if !p.noByID {
			mustSymlink(t, "../../"+name, filepath.Join(byID, fmt.Sprintf("nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456X-part%d", p.num)))
		}
		if !p.noUdev {
			// udev reports the disk's own table on every partition too.
			body := "E:ID_PART_TABLE_TYPE=gpt\nE:ID_PART_TABLE_UUID=7fc2333f-fc11-47b3-983f-e645d7894e74\nE:ID_PART_ENTRY_SCHEME=gpt\n"
			if p.partType != "" {
				body += "E:ID_PART_ENTRY_TYPE=" + p.partType + "\n"
			}
			if p.partUUID != "" {
				body += "E:ID_PART_ENTRY_UUID=" + p.partUUID + "\n"
			}
			if p.partName != "" {
				body += "E:ID_PART_ENTRY_NAME=" + p.partName + "\n"
			}
			if p.fsType != "" {
				body += "E:ID_FS_TYPE=" + p.fsType + "\n"
			}
			mustWriteFile(t, filepath.Join(udev, fmt.Sprintf("b259:%d", p.num)), body)
		}
	}

	mounts := filepath.Join(root, "mounts")
	mustWriteFile(t, mounts, "proc /proc proc rw 0 0\n/dev/nvme0n1p2 / ext4 rw 0 0\n/dev/nvme0n1p1 /boot/efi vfat rw 0 0\n")
	swapsFile := filepath.Join(root, "swaps")
	mustWriteFile(t, swapsFile, swaps)
	fstabFile := filepath.Join(root, "fstab")
	mustWriteFile(t, fstabFile, fstab)
	for name, body := range units {
		mustWriteFile(t, filepath.Join(unitDir, name), body)
	}

	return &Lister{
		SysBlockDir: sysBlock, ByIDDir: byID, ProcMounts: mounts, UdevDataDir: udev,
		SwapsFile: swapsFile, FstabFile: fstabFile, MountUnitDirs: []string{unitDir, filepath.Join(root, "absent-units")},
	}
}

func bootDisk(t *testing.T, l *Lister) Disk {
	t.Helper()
	disks, err := l.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, d := range disks {
		if d.Boot {
			return d
		}
	}
	t.Fatal("no boot disk in List")
	return Disk{}
}

func spare(num int) partFixture {
	return partFixture{num: num, sectors: 1800000000, partType: linuxDataGUID, partUUID: fmt.Sprintf("11111111-0000-0000-0000-00000000000%d", num)}
}

func baseParts() []partFixture {
	return []partFixture{
		{num: 1, sectors: 1048576, partType: "c12a7328-f81f-11d2-ba4b-00a0c93ec93b", partUUID: "aaaa0001", fsType: "vfat"},
		{num: 2, sectors: 117000000, partType: linuxDataGUID, partUUID: "aaaa0002", fsType: "ext4"},
	}
}

func TestLister_ReportsTheSparePartitionOfTheBootDiskAsACacheCandidate(t *testing.T) {
	parts := append(baseParts(), spare(3))
	l := newBootNVMeLister(t, parts, "Filename\tType\tSize\tUsed\tPriority\n", "# /etc/fstab\n/dev/nvme0n1p2 / ext4 defaults 0 1\n", nil)

	d := bootDisk(t, l)
	if len(d.CachePartitions) != 1 {
		t.Fatalf("CachePartitions = %+v, want exactly the spare partition", d.CachePartitions)
	}
	c := d.CachePartitions[0]
	if c.Device != "/dev/nvme0n1p3" || c.Size != 1800000000*512 || c.PartUUID != "11111111-0000-0000-0000-000000000003" ||
		c.ByIDName != "nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456X-part3" || c.Reason != ReasonSpareBootPartition {
		t.Fatalf("candidate = %+v", c)
	}
}

func TestLister_NeverReportsAPartitionThatCouldHoldData(t *testing.T) {
	swapPart := spare(4)
	swapPart.partType = "0657fd6d-a4ab-43c4-84e5-0933c84b4f4f"
	mountedPart := spare(5)
	signed := spare(6)
	signed.fsType = "ext4"
	fstabNamed := spare(7)
	byUUIDNamed := spare(8)
	held := spare(9)
	held.holders = []string{"dm-0"}
	espBlank := spare(10)
	espBlank.partType = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	noType := spare(11)
	noType.partType = ""
	noUUID := spare(12)
	noUUID.partUUID = ""
	noUdevData := spare(13)
	noUdevData.noUdev = true
	noHolders := spare(14)
	noHolders.noHolders = true
	noByID := spare(15)
	noByID.noByID = true
	unitNamed := spare(16)
	swapUnitNamed := spare(17)
	byIDNamed := spare(18)
	zeroSize := spare(19)
	zeroSize.sectors = 0
	labelNamed := spare(20)
	labelNamed.partName = "scratch"
	spacedLabelNamed := spare(21)
	spacedLabelNamed.partName = `my\x20scratch`
	byLabelNamed := spare(22)
	byLabelNamed.partName = "stash"
	unnamedBesideLabelSpec := spare(23)
	byLabelSpacedNamed := spare(24)
	byLabelSpacedNamed.partName = `my\x20stash`
	goodSpare := spare(3)
	goodSpare.partName = "cache-space"

	parts := append(baseParts(), goodSpare, swapPart, mountedPart, signed, fstabNamed, byUUIDNamed, held, espBlank, noType, noUUID, noUdevData, noHolders, noByID, unitNamed, swapUnitNamed, byIDNamed, zeroSize, labelNamed, spacedLabelNamed, byLabelNamed, unnamedBesideLabelSpec, byLabelSpacedNamed)
	swaps := "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n/dev/nvme0n1p4\tpartition\t8388604\t0\t-2\n"
	fstab := "/dev/nvme0n1p2 / ext4 defaults 0 1\n/dev/nvme0n1p7 /srv ext4 defaults 0 2\nPARTUUID=" + byUUIDNamed.partUUID + " /x ext4 defaults 0 2\n/dev/disk/by-id/nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456X-part18 /y ext4 defaults 0 2\nPARTLABEL=scratch /srv/scratch ext4 defaults,nofail 0 2\nPARTLABEL=my\\040scratch /srv/spaced ext4 defaults,nofail 0 2\n/dev/disk/by-partlabel/stash /srv/stash ext4 defaults,nofail 0 2\n/dev/disk/by-partlabel/my\\x20stash /srv/mystash ext4 defaults,nofail 0 2\nPARTLABEL=other /srv/other ext4 defaults,nofail 0 2\n"
	units := map[string]string{
		"srv-data.mount":    "[Mount]\nWhat=/dev/nvme0n1p16\nWhere=/srv/data\n",
		"dev-extra.swap":    "[Swap]\nWhat=/dev/nvme0n1p17\n",
		"unrelated.service": "[Service]\nExecStart=/bin/true\n",
	}
	l := newBootNVMeLister(t, parts, swaps, fstab, units)
	mustWriteFile(t, l.ProcMounts, "proc /proc proc rw 0 0\n/dev/nvme0n1p2 / ext4 rw 0 0\n/dev/nvme0n1p1 /boot/efi vfat rw 0 0\n/dev/nvme0n1p5 /mnt/other ext4 rw 0 0\n")

	d := bootDisk(t, l)
	var got []string
	for _, c := range d.CachePartitions {
		got = append(got, c.Device)
	}
	if len(got) != 1 || got[0] != "/dev/nvme0n1p3" {
		t.Fatalf("CachePartitions = %v, want only /dev/nvme0n1p3 — root, EFI, swap, mounted, fstab/unit-named, signed, held, wrong-type and identity-less partitions are all refused", got)
	}
}

func TestLister_FailsClosedWhenACandidateSourceIsUnreadable(t *testing.T) {
	parts := append(baseParts(), spare(3))
	for name, mutate := range map[string]func(*Lister){
		"swaps unreadable": func(l *Lister) { l.SwapsFile = filepath.Join(filepath.Dir(l.SwapsFile), "missing") },
		"fstab unreadable": func(l *Lister) { l.FstabFile = filepath.Join(filepath.Dir(l.FstabFile), "missing") },
		"swaps path unset": func(l *Lister) { l.SwapsFile = "" },
		"fstab path unset": func(l *Lister) { l.FstabFile = "" },
		"unit file unreadable": func(l *Lister) {
			dir := l.MountUnitDirs[0]
			if err := os.Mkdir(filepath.Join(dir, "broken.mount"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := newBootNVMeLister(t, parts, "Filename\tType\n", "/dev/nvme0n1p2 / ext4 defaults 0 1\n", nil)
			mutate(l)
			if d := bootDisk(t, l); len(d.CachePartitions) != 0 {
				t.Fatalf("CachePartitions = %+v, want none when a source cannot be read", d.CachePartitions)
			}
		})
	}
}

func TestLister_ReportsNoCandidatesOnANonBootDisk(t *testing.T) {
	l := newTestLister(t)
	disks, err := l.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range disks {
		if len(d.CachePartitions) != 0 {
			t.Fatalf("%s reports cache partitions %+v with no candidate sources configured", d.Device, d.CachePartitions)
		}
	}
}

func TestPartLabelMayMatch(t *testing.T) {
	for _, tc := range []struct {
		name, spec, udev string
		want             bool
	}{
		{"fstab octal blank against udev hex blank", `my\040scratch`, `my\x20scratch`, true},
		{"by-partlabel hex blank against udev hex blank", `my\x20scratch`, `my\x20scratch`, true},
		{"underscore is not udev's encoding of a blank", `my_scratch`, `my\x20scratch`, false},
		{"different label", `other`, `my\x20scratch`, false},
		{"no udev name cannot rule a spec out", `anything`, ``, true},
		{"undecodable udev name cannot rule a spec out", `anything`, `bad\zname`, true},
		{"encoded backslash in the label", `a\134b`, `a\x5cb`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := partLabelMayMatch(tc.spec, tc.udev); got != tc.want {
				t.Fatalf("partLabelMayMatch(%q, %q) = %v, want %v", tc.spec, tc.udev, got, tc.want)
			}
		})
	}
}
