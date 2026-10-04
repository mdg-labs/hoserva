package disk

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// parityInitFixture is an adopted array of two data disks, one former parity
// disk and a cache, as the inventory lists them.
type parityInitFixture struct {
	p    *FakeProvider
	plan AdoptionPlan
}

func newParityInitFixture(t *testing.T) *parityInitFixture {
	t.Helper()
	p := NewFakeProvider()
	p.AddDisk("/dev/sdb", Disk{Serial: "PAR1", WWN: "wwn-par1", ByIDName: "ata-EX_PAR1", Size: 8 << 40, Filesystem: "xfs", FSUUID: "old-parity-uuid"})
	p.AddDisk("/dev/sdc", Disk{Serial: "DAT1", WWN: "wwn-dat1", ByIDName: "ata-EX_DAT1", Size: 4 << 40, Filesystem: "xfs", FSUUID: "uuid-dat1"})
	p.AddDisk("/dev/sdd", Disk{Serial: "DAT2", WWN: "wwn-dat2", ByIDName: "ata-EX_DAT2", Size: 2 << 40, Filesystem: "ext4", FSUUID: "uuid-dat2"})
	p.AddDisk("/dev/nvme1n1", Disk{Serial: "CAC1", WWN: "wwn-cac1", ByIDName: "nvme-EX_CAC1", Size: 500 << 30, Filesystem: "btrfs", FSUUID: "old-cache-uuid"})
	plan := AdoptionPlan{
		Data: []AdoptedDisk{
			{AssignedDisk: AssignedDisk{Device: "/dev/sdc", Filesystem: XFS, Adopt: true, Serial: "DAT1", WWN: "wwn-dat1", ByIDName: "ata-EX_DAT1", FSUUID: "uuid-dat1"}, Size: 4 << 40},
			{AssignedDisk: AssignedDisk{Device: "/dev/sdd", Filesystem: EXT4, Adopt: true, Serial: "DAT2", WWN: "wwn-dat2", ByIDName: "ata-EX_DAT2", FSUUID: "uuid-dat2"}, Size: 2 << 40},
		},
		Parity: []RecordedDisk{{AssignedDisk: AssignedDisk{Device: "/dev/sdb", Filesystem: XFS, Serial: "PAR1", WWN: "wwn-par1", ByIDName: "ata-EX_PAR1"}, Size: 8 << 40}},
		Cache:  &RecordedDisk{AssignedDisk: AssignedDisk{Device: "/dev/nvme1n1", Filesystem: XFS, Serial: "CAC1", WWN: "wwn-cac1", ByIDName: "nvme-EX_CAC1"}, Size: 500 << 30},
	}
	return &parityInitFixture{p: p, plan: plan}
}

func byID(name string) string { return "/dev/disk/by-id/" + name }

func (f *parityInitFixture) noFormat(t *testing.T, why string) {
	t.Helper()
	if calls := f.p.FormatCalls(); len(calls) != 0 {
		t.Errorf("%s: formatted %v, want nothing erased", why, calls)
	}
}

func TestFormatParityInit_FormatsOnlyTheConfirmedParityAndCache(t *testing.T) {
	f := newParityInitFixture(t)
	confirmation := f.plan.ParityInitConfirmation()
	if confirmation != "ERASE /dev/nvme1n1, /dev/sdb" {
		t.Fatalf("confirmation = %q, want it to name every device it erases and no data disk", confirmation)
	}
	if err := FormatParityInit(context.Background(), f.p, nil, f.plan, confirmation); err != nil {
		t.Fatalf("FormatParityInit: %v", err)
	}
	want := []string{byID("ata-EX_PAR1"), byID("nvme-EX_CAC1")}
	if got := f.p.FormatCalls(); !reflect.DeepEqual(got, want) {
		t.Errorf("formatted %v, want exactly the parity disk and the cache through their by-id paths: %v", got, want)
	}
	for _, dev := range []string{"/dev/sdb", "/dev/nvme1n1"} {
		if fs, ok := f.p.FormattedAs(dev); !ok || fs != XFS {
			t.Errorf("%s formatted as %q (%v), want xfs: parity is always XFS (Q20)", dev, fs, ok)
		}
	}
	for _, dev := range []string{"/dev/sdc", "/dev/sdd"} {
		if _, ok := f.p.FormattedAs(dev); ok {
			t.Errorf("data disk %s was formatted", dev)
		}
	}
}

func TestFormatParityInit_RefusesAWrongOrMissingConfirmationAndErasesNothing(t *testing.T) {
	for name, confirmation := range map[string]string{
		"none":                            "",
		"another plan's":                  "ERASE /dev/sdb",
		"a data disk added":               "ERASE /dev/nvme1n1, /dev/sdb, /dev/sdc",
		"the right devices unsorted":      "ERASE /dev/sdb, /dev/nvme1n1",
		"the adoption-only phrase":        "ADOPT ONLY — NOTHING ERASED",
		"the right string, lower-cased":   "erase /dev/nvme1n1, /dev/sdb",
		"the right string with a suffix ": "ERASE /dev/nvme1n1, /dev/sdb ",
	} {
		t.Run(name, func(t *testing.T) {
			f := newParityInitFixture(t)
			err := FormatParityInit(context.Background(), f.p, nil, f.plan, confirmation)
			if !errors.Is(err, ErrConfirmationMismatch) {
				t.Fatalf("err = %v, want ErrConfirmationMismatch", err)
			}
			f.noFormat(t, "a refused confirmation")
		})
	}
}

// A refusal of the second target must not leave the first one erased: every
// target is resolved before the first mkfs.
func TestFormatParityInit_ARefusedTargetErasesNothingAtAll(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(f *parityInitFixture)
		want   error
	}{
		"the cache disk is gone": {
			change: func(f *parityInitFixture) {
				f.p = NewFakeProvider()
				f.p.AddDisk("/dev/sdb", Disk{Serial: "PAR1", WWN: "wwn-par1", ByIDName: "ata-EX_PAR1", Size: 8 << 40})
			},
			want: ErrAdoptDiskMissing,
		},
		"the cache disk is now the disk this machine boots from": {
			change: func(f *parityInitFixture) {
				f.p.AddDisk("/dev/nvme1n1", Disk{Serial: "CAC1", WWN: "wwn-cac1", ByIDName: "nvme-EX_CAC1", Size: 500 << 30, Boot: true})
			},
			want: ErrBootDevice,
		},
		"the cache disk is an Unraid boot device, which a whole-disk format would erase": {
			change: func(f *parityInitFixture) {
				f.p.AddDisk("/dev/nvme1n1", Disk{Serial: "CAC1", WWN: "wwn-cac1", ByIDName: "nvme-EX_CAC1", Size: 500 << 30, UnraidBoot: true})
			},
			want: ErrUnraidCacheNotPartition,
		},
		"the parity disk is now the Unraid stick": {
			change: func(f *parityInitFixture) {
				f.p.AddDisk("/dev/sdb", Disk{Serial: "PAR1", WWN: "wwn-par1", ByIDName: "ata-EX_PAR1", Size: 8 << 40, Filesystem: UnraidStickFilesystem, Label: UnraidStickLabel})
			},
			want: ErrUnraidStick,
		},
		"the parity disk's by-id name changed": {
			change: func(f *parityInitFixture) {
				f.p.AddDisk("/dev/sdb", Disk{Serial: "PAR1", WWN: "wwn-par1", ByIDName: "ata-OTHER_PAR1", Size: 8 << 40})
			},
			want: ErrDiskIdentityChanged,
		},
		"two disks now carry the cache's identity": {
			change: func(f *parityInitFixture) {
				f.p.AddDisk("/dev/nvme2n1", Disk{Serial: "CAC1", WWN: "wwn-cac1", ByIDName: "nvme-EX_CAC1", Size: 500 << 30})
			},
			want: ErrAdoptDiskAmbiguous,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newParityInitFixture(t)
			tc.change(f)
			err := FormatParityInit(context.Background(), f.p, nil, f.plan, f.plan.ParityInitConfirmation())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			f.noFormat(t, "a refused target")
		})
	}
}

func TestFormatParityInit_RefusesAPlanThatBreaksTheParityRules(t *testing.T) {
	f := newParityInitFixture(t)
	f.plan.Parity[0].Size = 1 << 40
	if err := FormatParityInit(context.Background(), f.p, nil, f.plan, f.plan.ParityInitConfirmation()); !errors.Is(err, ErrParityTooSmall) {
		t.Fatalf("err = %v, want ErrParityTooSmall (Q20)", err)
	}
	f.noFormat(t, "a parity disk smaller than the largest data disk")

	f = newParityInitFixture(t)
	f.plan.Parity[0].WeakIdentity = true
	if err := FormatParityInit(context.Background(), f.p, nil, f.plan, f.plan.ParityInitConfirmation()); !errors.Is(err, ErrWeakIdentityParity) {
		t.Fatalf("err = %v, want ErrWeakIdentityParity (Q21)", err)
	}
	f.noFormat(t, "a weak-identity parity disk")
}

// unraidBootFixture is the cache on partition 4 of an Unraid internal boot
// device that shares its disk with the cache.
func unraidBootFixture(t *testing.T) *parityInitFixture {
	t.Helper()
	f := newParityInitFixture(t)
	part := &BootPartition{Device: "/dev/nvme1n1p4", Size: 400 << 30, ByIDName: "nvme-EX_CAC1-part4", PartUUID: "aaaa-4444"}
	f.p.AddDisk("/dev/nvme1n1", Disk{Serial: "CAC1", WWN: "wwn-cac1", ByIDName: "nvme-EX_CAC1", Size: 500 << 30, UnraidBoot: true, UnraidDataPartition: part})
	f.plan.Cache = &RecordedDisk{
		AssignedDisk: AssignedDisk{Device: part.Device, Filesystem: XFS, Serial: "CAC1", WWN: "wwn-cac1", ByIDName: part.ByIDName, PartUUID: part.PartUUID},
		Size:         part.Size,
	}
	return f
}

func TestFormatParityInit_AnUnraidBootDevicesCacheIsItsDataPartitionOnly(t *testing.T) {
	f := unraidBootFixture(t)
	confirmation := f.plan.ParityInitConfirmation()
	if confirmation != "ERASE /dev/nvme1n1p4, /dev/sdb" {
		t.Fatalf("confirmation = %q, want the partition named, not the disk", confirmation)
	}
	if err := FormatParityInit(context.Background(), f.p, nil, f.plan, confirmation); err != nil {
		t.Fatalf("FormatParityInit: %v", err)
	}
	want := []string{byID("ata-EX_PAR1"), byID("nvme-EX_CAC1-part4")}
	if got := f.p.FormatCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("formatted %v, want the parity disk and partition 4 only: %v", got, want)
	}
	if _, ok := f.p.FormattedAs("/dev/nvme1n1"); ok {
		t.Error("the Unraid boot device itself was formatted: partitions 1 to 3 are Unraid's boot pool")
	}
	if fs, ok := f.p.FormattedAs("/dev/nvme1n1p4"); !ok || fs != XFS {
		t.Errorf("partition 4 formatted as %q (%v), want xfs", fs, ok)
	}
}

func TestFormatParityInit_ARefusedUnraidBootCacheErasesNothingAtAll(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(f *parityInitFixture)
		want   error
	}{
		"a second Unraid boot device is attached, which may be a mirrored pair": {
			change: func(f *parityInitFixture) {
				f.p.AddDisk("/dev/nvme2n1", Disk{Serial: "CAC2", WWN: "wwn-cac2", ByIDName: "nvme-EX_CAC2", Size: 500 << 30, UnraidBoot: true,
					UnraidDataPartition: &BootPartition{Device: "/dev/nvme2n1p4", Size: 400 << 30, ByIDName: "nvme-EX_CAC2-part4", PartUUID: "bbbb-4444"}})
			},
			want: ErrMirroredBootCache,
		},
		"the partition's PARTUUID changed": {
			change: func(f *parityInitFixture) {
				f.p.AddDisk("/dev/nvme1n1", Disk{Serial: "CAC1", WWN: "wwn-cac1", ByIDName: "nvme-EX_CAC1", Size: 500 << 30, UnraidBoot: true,
					UnraidDataPartition: &BootPartition{Device: "/dev/nvme1n1p4", Size: 400 << 30, ByIDName: "nvme-EX_CAC1-part4", PartUUID: "cccc-4444"}})
			},
			want: ErrAdoptDiskChanged,
		},
		"the disk is no longer an Unraid boot device": {
			change: func(f *parityInitFixture) {
				f.p.AddDisk("/dev/nvme1n1", Disk{Serial: "CAC1", WWN: "wwn-cac1", ByIDName: "nvme-EX_CAC1", Size: 500 << 30})
			},
			want: ErrBootPartitionNotSpare,
		},
		"the disk is the one this machine boots from": {
			change: func(f *parityInitFixture) {
				f.p.AddDisk("/dev/nvme1n1", Disk{Serial: "CAC1", WWN: "wwn-cac1", ByIDName: "nvme-EX_CAC1", Size: 500 << 30, Boot: true, UnraidBoot: true,
					UnraidDataPartition: &BootPartition{Device: "/dev/nvme1n1p4", Size: 400 << 30, ByIDName: "nvme-EX_CAC1-part4", PartUUID: "aaaa-4444"}})
			},
			want: ErrBootPartitionNotSpare,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := unraidBootFixture(t)
			tc.change(f)
			err := FormatParityInit(context.Background(), f.p, nil, f.plan, f.plan.ParityInitConfirmation())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			f.noFormat(t, "a refused Unraid boot cache")
		})
	}
}

func TestFormatParityInit_APartitionWithNoStableIdentityIsNeverFormatted(t *testing.T) {
	f := unraidBootFixture(t)
	f.plan.Cache.PartUUID = ""
	f.plan.Cache.ByIDName = ""
	err := FormatParityInit(context.Background(), f.p, nil, f.plan, f.plan.ParityInitConfirmation())
	if err == nil || !strings.Contains(err.Error(), "partition") {
		t.Fatalf("err = %v, want a refusal of a partition with no partition identity", err)
	}
	f.noFormat(t, "a cache partition with no PARTUUID")
}

// A cache on a spare partition of the disk this machine boots from is formatted
// only when the partition is positively blank, and never the boot disk.
func TestFormatParityInit_ASpareBootPartitionIsFormattedOnlyWhenBlank(t *testing.T) {
	build := func() (*parityInitFixture, *FakeBlankProber, string) {
		f := newParityInitFixture(t)
		part := CachePartition{Device: "/dev/nvme0n1p3", Size: 400 << 30, ByIDName: "nvme-BOOT-part3", PartUUID: "dddd-3333", Reason: ReasonSpareBootPartition}
		f.p.AddDisk("/dev/nvme0n1", Disk{Serial: "BOOT1", WWN: "wwn-boot1", ByIDName: "nvme-BOOT", Size: 1 << 40, Boot: true, CachePartitions: []CachePartition{part}})
		f.plan.Cache = &RecordedDisk{
			AssignedDisk: AssignedDisk{Device: part.Device, Filesystem: XFS, Serial: "BOOT1", WWN: "wwn-boot1", ByIDName: part.ByIDName, PartUUID: part.PartUUID},
			Size:         part.Size,
		}
		return f, NewFakeBlankProber(), byID(part.ByIDName)
	}

	f, probe, target := build()
	probe.ScriptBlank(target)
	if err := FormatParityInit(context.Background(), f.p, probe, f.plan, f.plan.ParityInitConfirmation()); err != nil {
		t.Fatalf("FormatParityInit: %v", err)
	}
	if got, want := f.p.FormatCalls(), []string{byID("ata-EX_PAR1"), target}; !reflect.DeepEqual(got, want) {
		t.Errorf("formatted %v, want %v", got, want)
	}
	if _, ok := f.p.FormattedAs("/dev/nvme0n1"); ok {
		t.Error("the boot disk was formatted")
	}

	f, probe, target = build()
	probe.ScriptFound(target)
	if err := FormatParityInit(context.Background(), f.p, probe, f.plan, f.plan.ParityInitConfirmation()); !errors.Is(err, ErrBootPartitionNotBlank) {
		t.Fatalf("err = %v, want ErrBootPartitionNotBlank", err)
	}
	f.noFormat(t, "a partition that holds a signature")

	f, _, _ = build()
	if err := FormatParityInit(context.Background(), f.p, nil, f.plan, f.plan.ParityInitConfirmation()); !errors.Is(err, ErrBootPartitionNotBlank) {
		t.Fatalf("err = %v, want ErrBootPartitionNotBlank with no probe", err)
	}
	f.noFormat(t, "no blank probe")
}
