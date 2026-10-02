package disk

import (
	"context"
	"errors"
	"slices"
	"testing"
)

const (
	bootNVMe        = "/dev/nvme0n1"
	bootEFIPart     = "/dev/nvme0n1p1"
	bootRootPart    = "/dev/nvme0n1p2"
	bootCachePart   = "/dev/nvme0n1p3"
	bootByIDPrefix  = "nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456X"
	bootCacheByID   = bootByIDPrefix + "-part3"
	bootCachePUUID  = "5b3d9e0a-03"
	bootCachePath   = "/dev/disk/by-id/" + bootCacheByID
	dataDiskDevice1 = "/dev/sdb"
	dataDiskDevice2 = "/dev/sdc"
	parityDiskDev   = "/dev/sdd"
)

// sharedNVMeProvider is the layout this issue allows: one NVMe holding
// the root filesystem and a spare partition. Its partition table is
// never modelled as writable — the only thing the fake can record is a
// Format call, and FormatCalls is what every test below asserts on.
func sharedNVMeProvider() *FakeProvider {
	p := NewFakeProvider()
	p.AddDisk(bootNVMe, Disk{
		Size: 1 * TB, Boot: true, Serial: "S4EWNX0M123456X", ByIDName: bootByIDPrefix,
		CachePartitions: []CachePartition{{
			Device: bootCachePart, Size: 900 * GB, ByIDName: bootCacheByID, PartUUID: bootCachePUUID,
			Reason: ReasonSpareBootPartition,
		}},
	})
	p.AddDisk(parityDiskDev, Disk{Size: 8 * TB, WWN: "0xp", ByIDName: "wwn-0xp"})
	p.AddDisk(dataDiskDevice1, Disk{Size: 4 * TB, WWN: "0x1", ByIDName: "wwn-0x1"})
	p.AddDisk(dataDiskDevice2, Disk{Size: 4 * TB, WWN: "0x2", ByIDName: "wwn-0x2"})
	return p
}

func cachePartitionAssignment() AssignedDisk {
	return AssignedDisk{
		Device: bootCachePart, Filesystem: EXT4, Serial: "S4EWNX0M123456X",
		ByIDName: bootCacheByID, PartUUID: bootCachePUUID,
	}
}

func sharedNVMePlan(cache AssignedDisk) TopologyPlan {
	return TopologyPlan{
		Parity: []AssignedDisk{{Device: parityDiskDev, Filesystem: XFS, WWN: "0xp", ByIDName: "wwn-0xp"}},
		Data: []AssignedDisk{
			{Device: dataDiskDevice1, Filesystem: XFS, WWN: "0x1", ByIDName: "wwn-0x1"},
			{Device: dataDiskDevice2, Filesystem: XFS, WWN: "0x2", ByIDName: "wwn-0x2"},
		},
		Cache: &cache,
	}
}

func sharedNVMeSizes() map[string]int64 {
	return map[string]int64{
		parityDiskDev: 8 * TB, dataDiskDevice1: 4 * TB, dataDiskDevice2: 4 * TB, bootCachePart: 900 * GB,
	}
}

// formatViolations is the harness's own "a write landed outside the
// target" detector: every Format call the fake recorded that is not in
// allowed. The control arm below proves it reports a violation rather
// than passing vacuously.
func formatViolations(p *FakeProvider, allowed ...string) []string {
	var out []string
	for _, c := range p.FormatCalls() {
		if !slices.Contains(allowed, c) {
			out = append(out, c)
		}
	}
	return out
}

func TestFormatViolations_ControlArmDetectsAWriteOutsideTheTarget(t *testing.T) {
	p := sharedNVMeProvider()
	if err := p.Format(context.Background(), dataDiskDevice1, XFS); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if got := formatViolations(p, bootCachePath); len(got) != 1 || got[0] != dataDiskDevice1 {
		t.Fatalf("formatViolations = %v, want [%s] — the harness must flag a write outside the allowed target", got, dataDiskDevice1)
	}
}

func TestFormatPlan_FormatsOnlyTheBlankSparePartition(t *testing.T) {
	p := sharedNVMeProvider()
	probe := NewFakeBlankProber()
	probe.ScriptBlank(bootCachePath)
	plan := sharedNVMePlan(cachePartitionAssignment())

	if err := FormatPlanProbed(context.Background(), p, NewFakeRunner(), probe, plan, sharedNVMeSizes(), plan.Confirmation()); err != nil {
		t.Fatalf("FormatPlanProbed: %v", err)
	}
	if got := formatViolations(p, "/dev/disk/by-id/wwn-0xp", "/dev/disk/by-id/wwn-0x1", "/dev/disk/by-id/wwn-0x2", bootCachePath); len(got) != 0 {
		t.Fatalf("formatted outside the assigned disks and the spare partition: %v", got)
	}
	if fs, ok := p.FormattedAs(bootCachePart); !ok || fs != EXT4 {
		t.Fatalf("FormattedAs(%s) = %q, %v, want ext4", bootCachePart, fs, ok)
	}
	if _, ok := p.FormattedAs(bootNVMe); ok {
		t.Fatalf("the whole boot disk %s was formatted", bootNVMe)
	}
	if got := probe.Probed(); !slices.Contains(got, bootCachePath) {
		t.Fatalf("blank probe ran against %v, want it to include %s", got, bootCachePath)
	}
}

func TestFormatPlan_RefusesEveryPartitionThatIsNotTheSpare(t *testing.T) {
	cases := []struct {
		name    string
		assign  AssignedDisk
		wantErr error
	}{
		{"root partition", AssignedDisk{Device: bootRootPart, Filesystem: EXT4, Serial: "S4EWNX0M123456X", ByIDName: bootByIDPrefix + "-part2", PartUUID: "5b3d9e0a-02"}, ErrBootPartitionNotSpare},
		{"EFI partition", AssignedDisk{Device: bootEFIPart, Filesystem: EXT4, Serial: "S4EWNX0M123456X", ByIDName: bootByIDPrefix + "-part1", PartUUID: "5b3d9e0a-01"}, ErrBootPartitionNotSpare},
		{"spare partition with the wrong PARTUUID", AssignedDisk{Device: bootCachePart, Filesystem: EXT4, Serial: "S4EWNX0M123456X", ByIDName: bootCacheByID, PartUUID: "ffffffff-03"}, ErrBootPartitionNotSpare},
		{"spare partition with no PARTUUID", AssignedDisk{Device: bootCachePart, Filesystem: EXT4, Serial: "S4EWNX0M123456X", ByIDName: bootCacheByID}, ErrBootPartitionNotSpare},
		{"spare partition with no by-id name", AssignedDisk{Device: bootCachePart, Filesystem: EXT4, Serial: "S4EWNX0M123456X", PartUUID: bootCachePUUID}, ErrBootPartitionNotSpare},
		{"the whole boot disk", AssignedDisk{Device: bootNVMe, Filesystem: EXT4, Serial: "S4EWNX0M123456X", ByIDName: bootByIDPrefix}, ErrBootDevice},
		{"an adopted spare partition", func() AssignedDisk { a := cachePartitionAssignment(); a.Adopt = true; return a }(), ErrBootPartitionAdopt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := sharedNVMeProvider()
			probe := NewFakeBlankProber()
			probe.ScriptBlank(bootCachePath)
			plan := sharedNVMePlan(tc.assign)
			sizes := sharedNVMeSizes()
			sizes[tc.assign.Device] = 100 * GB

			err := FormatPlanProbed(context.Background(), p, NewFakeRunner(), probe, plan, sizes, plan.Confirmation())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("FormatPlanProbed: got %v, want %v", err, tc.wantErr)
			}
			if calls := p.FormatCalls(); len(calls) != 0 {
				t.Fatalf("a refused plan still formatted %v — nothing may be erased when any assigned disk is refused", calls)
			}
		})
	}
}

func TestFormatPlan_RefusesASparePartitionThatCarriesASignature(t *testing.T) {
	for name, script := range map[string]func(*FakeBlankProber){
		"signature found":     func(f *FakeBlankProber) { f.ScriptFound(bootCachePath) },
		"probe error":         func(f *FakeBlankProber) { f.ScriptError(bootCachePath, errors.New("blkid: ambiguous")) },
		"nothing scripted":    func(f *FakeBlankProber) {},
		"probe of other path": func(f *FakeBlankProber) { f.ScriptBlank(bootCachePart) },
	} {
		t.Run(name, func(t *testing.T) {
			p := sharedNVMeProvider()
			probe := NewFakeBlankProber()
			script(probe)
			plan := sharedNVMePlan(cachePartitionAssignment())

			err := FormatPlanProbed(context.Background(), p, NewFakeRunner(), probe, plan, sharedNVMeSizes(), plan.Confirmation())
			if !errors.Is(err, ErrBootPartitionNotBlank) {
				t.Fatalf("FormatPlanProbed: got %v, want ErrBootPartitionNotBlank", err)
			}
			if calls := p.FormatCalls(); len(calls) != 0 {
				t.Fatalf("formatted %v although the partition was not confirmed blank — the parity and data disks must not be erased either", calls)
			}
		})
	}
}

func TestFormatPlan_RefusesASparePartitionWhenNoProbeIsAvailable(t *testing.T) {
	p := sharedNVMeProvider()
	plan := sharedNVMePlan(cachePartitionAssignment())

	err := FormatPlanProbed(context.Background(), p, NewFakeRunner(), nil, plan, sharedNVMeSizes(), plan.Confirmation())
	if !errors.Is(err, ErrBootPartitionNotBlank) {
		t.Fatalf("FormatPlanProbed(nil probe): got %v, want ErrBootPartitionNotBlank (fail closed)", err)
	}
	if calls := p.FormatCalls(); len(calls) != 0 {
		t.Fatalf("formatted %v with no probe to confirm the partition blank", calls)
	}
}

// TestFormatPlan_RefusesAPartitionThatStoppedQualifying covers a
// partition that was a candidate when the plan was built and has since
// been mounted, swapped on or given a filesystem: List no longer reports
// it as a candidate, so the format must be refused.
func TestFormatPlan_RefusesAPartitionThatStoppedQualifying(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk(bootNVMe, Disk{Size: 1 * TB, Boot: true, Serial: "S4EWNX0M123456X", ByIDName: bootByIDPrefix})
	p.AddDisk(parityDiskDev, Disk{Size: 8 * TB, WWN: "0xp", ByIDName: "wwn-0xp"})
	p.AddDisk(dataDiskDevice1, Disk{Size: 4 * TB, WWN: "0x1", ByIDName: "wwn-0x1"})
	p.AddDisk(dataDiskDevice2, Disk{Size: 4 * TB, WWN: "0x2", ByIDName: "wwn-0x2"})
	probe := NewFakeBlankProber()
	probe.ScriptBlank(bootCachePath)
	plan := sharedNVMePlan(cachePartitionAssignment())

	err := FormatPlanProbed(context.Background(), p, NewFakeRunner(), probe, plan, sharedNVMeSizes(), plan.Confirmation())
	if !errors.Is(err, ErrBootPartitionNotSpare) {
		t.Fatalf("FormatPlanProbed: got %v, want ErrBootPartitionNotSpare", err)
	}
	if calls := p.FormatCalls(); len(calls) != 0 {
		t.Fatalf("formatted %v", calls)
	}
	if got := probe.Probed(); len(got) != 0 {
		t.Fatalf("blank probe ran against %v for a partition that is not a candidate", got)
	}
}

func TestFormatPlan_RefusesABootDiskPartitionAsDataOrParity(t *testing.T) {
	for name, build := range map[string]func(AssignedDisk) TopologyPlan{
		"data": func(a AssignedDisk) TopologyPlan {
			plan := sharedNVMePlan(AssignedDisk{Device: dataDiskDevice2, Filesystem: EXT4})
			plan.Data = append(plan.Data, a)
			return plan
		},
		"parity": func(a AssignedDisk) TopologyPlan {
			plan := sharedNVMePlan(AssignedDisk{Device: dataDiskDevice2, Filesystem: EXT4})
			a.Filesystem = XFS
			plan.Parity = append(plan.Parity, a)
			return plan
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := sharedNVMeProvider()
			probe := NewFakeBlankProber()
			probe.ScriptBlank(bootCachePath)
			plan := build(cachePartitionAssignment())
			if err := CheckFormatTargets(plan); !errors.Is(err, ErrUnmanagedDevice) {
				t.Fatalf("CheckFormatTargets: got %v, want ErrUnmanagedDevice", err)
			}
			if err := FormatAssignedProbed(context.Background(), p, probe, plan, bootCachePart, EXT4); !errors.Is(err, ErrUnmanagedDevice) {
				t.Fatalf("FormatAssignedProbed: got %v, want ErrUnmanagedDevice", err)
			}
			if calls := p.FormatCalls(); len(calls) != 0 {
				t.Fatalf("formatted %v", calls)
			}
		})
	}
}

func TestFakeProvider_FormatRefusesAPartitionWithoutTheCacheException(t *testing.T) {
	p := sharedNVMeProvider()
	for _, dev := range []string{bootCachePart, bootCachePath, bootRootPart, bootNVMe} {
		err := p.Format(context.Background(), dev, EXT4)
		if err == nil {
			t.Fatalf("Format(%s) with no cache-partition exception succeeded", dev)
		}
	}
	if calls := p.FormatCalls(); len(calls) != 0 {
		t.Fatalf("recorded successful formats %v", calls)
	}
}

func TestIsPartition(t *testing.T) {
	for _, tc := range []struct {
		device, byID string
		want         bool
	}{
		{"/dev/nvme0n1p3", "", true},
		{"/dev/nvme0n1", "", false},
		{"/dev/sda2", "", true},
		{"/dev/sda", "", false},
		{"/dev/loop5p2", "", true},
		{"/dev/loop5", "", false},
		{"/dev/sda", "wwn-0x1-part3", true},
		{"/dev/sda", "wwn-0x1", false},
		{"/dev/mmcblk0p1", "", true},
	} {
		if got := IsPartition(tc.device, tc.byID); got != tc.want {
			t.Errorf("IsPartition(%q, %q) = %v, want %v", tc.device, tc.byID, got, tc.want)
		}
	}
}

func TestFormatCommandForBootPartitionNeverNamesAPartitionTool(t *testing.T) {
	for _, fs := range []FilesystemType{XFS, EXT4, BTRFS} {
		argv, err := formatCommand(bootCachePath, fs)
		if err != nil {
			t.Fatalf("formatCommand(%s): %v", fs, err)
		}
		switch argv[0] {
		case "mkfs.xfs", "mkfs.ext4", "mkfs.btrfs":
		default:
			t.Errorf("formatCommand(%s) runs %q", fs, argv[0])
		}
		if last := argv[len(argv)-1]; last != bootCachePath {
			t.Errorf("formatCommand(%s) ends with %q, want the partition path only", fs, last)
		}
	}
}
