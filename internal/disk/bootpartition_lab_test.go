//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host. It proves the boot-disk cache partition's format guard
// against real loop devices, a real blkid and a real mkfs.
//
// The lab image has no partitioning tool and a loop device inside the lab
// container has no partition nodes, so each partition of the stand-in boot
// disk is its own loop device, reached through a /dev/disk/by-id/...-partN
// link made inside the container's own /dev — the path Hoserva formats a
// partition through. The boot disk and its other partitions are loop
// devices too; the proof of "writes nothing else" is a SHA-256 of every
// one of them before and after.

package disk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordingRunner delegates to the real CommandRunner and records every
// argv, so the test can assert which tools Hoserva's own code ran.
type recordingRunner struct {
	mu    sync.Mutex
	calls []RunCall
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, RunCall{Name: name, Args: append([]string(nil), args...)})
	r.mu.Unlock()
	return CommandRunner{}.Run(ctx, name, args...)
}

func (r *recordingRunner) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		out = append(out, c.Name)
	}
	return out
}

// forbiddenPartitionTools are the tools that would write the boot disk's
// partition table or erase signatures from it.
var forbiddenPartitionTools = []string{"sgdisk", "parted", "sfdisk", "fdisk", "wipefs", "partprobe", "blkdiscard"}

// forbiddenToolCalls is the harness's detector for a partitioning or
// wiping tool in calls; the control arm proves it fires.
func forbiddenToolCalls(calls []RunCall) []string {
	var bad []string
	for _, c := range calls {
		for _, f := range forbiddenPartitionTools {
			if c.Name == f {
				bad = append(bad, c.Name)
			}
		}
	}
	return bad
}

func hashDevice(t *testing.T, dev string) string {
	t.Helper()
	f, err := os.Open(dev)
	if err != nil {
		t.Fatalf("opening %s: %v", dev, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hashing %s: %v", dev, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeAt(t *testing.T, dev string, off int64, data []byte) {
	t.Helper()
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", dev, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt(data, off); err != nil {
		t.Fatalf("writing %s: %v", dev, err)
	}
}

// changedDevices returns every device in before whose hash now differs.
func changedDevices(t *testing.T, before map[string]string) []string {
	t.Helper()
	var changed []string
	for dev, sum := range before {
		if hashDevice(t, dev) != sum {
			changed = append(changed, dev)
		}
	}
	return changed
}

func linkByID(t *testing.T, name, dev string) {
	t.Helper()
	dir := "/dev/disk/by-id"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	link := filepath.Join(dir, name)
	_ = os.Remove(link)
	if err := os.Symlink(dev, link); err != nil {
		t.Fatalf("linking %s -> %s: %v", link, dev, err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

func TestLabBootCachePartition_FormatsOnlyTheBlankSparePartition(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	rec := &recordingRunner{}
	plain := CommandRunner{}

	boot := createLoopImage(ctx, t, plain, lab, "bootcache-boot")
	efi := createLoopImage(ctx, t, plain, lab, "bootcache-efi")
	root := createLoopImage(ctx, t, plain, lab, "bootcache-root")
	swap := createLoopImage(ctx, t, plain, lab, "bootcache-swap")
	spareDev := createLoopImage(ctx, t, plain, lab, "bootcache-spare")
	staleExt4 := createLoopImage(ctx, t, plain, lab, "bootcache-stale-ext4")
	staleSwap := createLoopImage(ctx, t, plain, lab, "bootcache-stale-swap")
	parityDev := createLoopImage(ctx, t, plain, lab, "bootcache-parity")
	dataDev := createLoopImage(ctx, t, plain, lab, "bootcache-data")

	// The stand-in boot disk carries a protective MBR and a GPT header
	// signature, the bytes a partitioning tool would rewrite.
	writeAt(t, boot, 510, []byte{0x55, 0xaa})
	writeAt(t, boot, 512, []byte("EFI PART"))
	writeAt(t, efi, 0, []byte("EFI-SYSTEM-PARTITION-MARKER"))
	if _, err := plain.Run(ctx, "mkfs.ext4", "-q", root); err != nil {
		t.Fatalf("mkfs.ext4 %s: %v", root, err)
	}
	if _, err := plain.Run(ctx, "mkswap", swap); err != nil {
		t.Fatalf("mkswap %s: %v", swap, err)
	}
	if _, err := plain.Run(ctx, "mkfs.ext4", "-q", staleExt4); err != nil {
		t.Fatalf("mkfs.ext4 %s: %v", staleExt4, err)
	}
	if _, err := plain.Run(ctx, "mkswap", staleSwap); err != nil {
		t.Fatalf("mkswap %s: %v", staleSwap, err)
	}

	const parent = "nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456X"
	for n, dev := range map[int]string{1: efi, 2: root, 3: spareDev, 4: swap, 5: staleExt4, 6: staleSwap} {
		linkByID(t, fmt.Sprintf("%s-part%d", parent, n), dev)
	}

	// udev's cached view: p5 and p6 look like blank spare partitions (the
	// cache is stale), while their real content is an ext4 filesystem and
	// swap. The probe at format time is what has to refuse them.
	parts := append(baseParts(), spare(3), partFixture{num: 4, sectors: 1800000000, partType: "0657fd6d-a4ab-43c4-84e5-0933c84b4f4f", partUUID: "aaaa0004"}, spare(5), spare(6))
	lister := newBootNVMeLister(t, parts, "Filename\tType\n", "/dev/nvme0n1p2 / ext4 defaults 0 1\n", nil)
	provider := &LinuxProvider{Lister: lister, Exec: rec}
	probe := LinuxBlankProber{Exec: rec}

	candidates := bootDisk(t, lister).CachePartitions
	var got []string
	for _, c := range candidates {
		got = append(got, c.Device)
	}
	if strings.Join(got, ",") != "/dev/nvme0n1p3,/dev/nvme0n1p5,/dev/nvme0n1p6" {
		t.Fatalf("candidates = %v, want the spare and the two stale-cache look-alikes only", got)
	}

	untouchable := map[string]string{}
	for _, dev := range []string{boot, efi, root, swap, staleExt4, staleSwap} {
		untouchable[dev] = hashDevice(t, dev)
	}

	assign := func(n int) AssignedDisk {
		return AssignedDisk{
			Device: fmt.Sprintf("/dev/nvme0n1p%d", n), Filesystem: EXT4, Serial: "S4EWNX0M123456X",
			ByIDName: fmt.Sprintf("%s-part%d", parent, n), PartUUID: fmt.Sprintf("11111111-0000-0000-0000-00000000000%d", n),
		}
	}

	t.Run("refuses what is not, or is no longer, a blank spare partition", func(t *testing.T) {
		cases := []struct {
			name    string
			cache   AssignedDisk
			wantErr error
		}{
			{"root partition", AssignedDisk{Device: "/dev/nvme0n1p2", Filesystem: EXT4, Serial: "S4EWNX0M123456X", ByIDName: parent + "-part2", PartUUID: "aaaa0002"}, ErrBootPartitionNotSpare},
			{"EFI partition", AssignedDisk{Device: "/dev/nvme0n1p1", Filesystem: EXT4, Serial: "S4EWNX0M123456X", ByIDName: parent + "-part1", PartUUID: "aaaa0001"}, ErrBootPartitionNotSpare},
			{"swap partition", AssignedDisk{Device: "/dev/nvme0n1p4", Filesystem: EXT4, Serial: "S4EWNX0M123456X", ByIDName: parent + "-part4", PartUUID: "aaaa0004"}, ErrBootPartitionNotSpare},
			{"a partition holding an ext4 filesystem", assign(5), ErrBootPartitionNotBlank},
			{"a partition holding swap", assign(6), ErrBootPartitionNotBlank},
			{"the whole boot disk", AssignedDisk{Device: "/dev/nvme0n1", Filesystem: EXT4, Serial: "S4EWNX0M123456X", ByIDName: parent}, ErrBootDevice},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cache := tc.cache
				plan := TopologyPlan{Cache: &cache}
				err := FormatAssignedProbed(ctx, provider, probe, plan, cache.Device, EXT4)
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("FormatAssignedProbed: got %v, want %v", err, tc.wantErr)
				}
			})
		}
		if changed := changedDevices(t, untouchable); len(changed) != 0 {
			t.Fatalf("refused formats still changed %v", changed)
		}
		for _, name := range rec.names() {
			if strings.HasPrefix(name, "mkfs") {
				t.Fatalf("a refused format ran %s", name)
			}
		}
	})

	t.Run("formats exactly the blank spare partition", func(t *testing.T) {
		cache := assign(3)
		plan := TopologyPlan{
			Parity: []AssignedDisk{{Device: parityDev, Filesystem: XFS}},
			Data:   []AssignedDisk{{Device: dataDev, Filesystem: XFS}},
			Cache:  &cache,
		}
		sizes := map[string]int64{parityDev: 320 << 20, dataDev: 320 << 20, cache.Device: 1800000000 * 512}
		if err := FormatPlanProbed(ctx, provider, rec, probe, plan, sizes, plan.Confirmation()); err != nil {
			t.Fatalf("FormatPlanProbed: %v", err)
		}
		if got := blkidType(t, ctx, plain, spareDev); got != "ext4" {
			t.Fatalf("spare partition filesystem = %q, want ext4", got)
		}
		if got := blkidType(t, ctx, plain, parityDev); got != "xfs" {
			t.Fatalf("parity disk filesystem = %q, want xfs", got)
		}
		if changed := changedDevices(t, untouchable); len(changed) != 0 {
			t.Fatalf("the format wrote outside its targets: %v changed", changed)
		}
		if bad := forbiddenToolCalls(rec.calls); len(bad) != 0 {
			t.Fatalf("ran partitioning or wiping tools: %v", bad)
		}
		var mkfsTargets []string
		for _, c := range rec.calls {
			if strings.HasPrefix(c.Name, "mkfs") {
				mkfsTargets = append(mkfsTargets, c.Args[len(c.Args)-1])
			}
			switch c.Name {
			case "blkid", "mkfs.ext4", "mkfs.xfs":
			default:
				t.Fatalf("ran unexpected tool %q", c.Name)
			}
		}
		want := "/dev/disk/by-id/" + parent + "-part3"
		found := false
		for _, target := range mkfsTargets {
			if target == want {
				found = true
			}
			if target == "/dev/nvme0n1" || target == boot || strings.Contains(target, "-part1") || strings.Contains(target, "-part2") {
				t.Fatalf("mkfs ran against %s", target)
			}
		}
		if !found {
			t.Fatalf("no mkfs ran against %s; targets = %v", want, mkfsTargets)
		}
	})

	t.Run("control arm: the harness detects a write outside the target and a partitioning tool", func(t *testing.T) {
		writeAt(t, root, 4096, []byte{0xde, 0xad})
		changed := changedDevices(t, untouchable)
		if len(changed) != 1 || changed[0] != root {
			t.Fatalf("changedDevices = %v, want only %s — the checksum harness must detect an outside write", changed, root)
		}
		if bad := forbiddenToolCalls([]RunCall{{Name: "sgdisk"}, {Name: "mkfs.ext4"}}); len(bad) != 1 {
			t.Fatalf("forbiddenToolCalls = %v, want it to flag sgdisk", bad)
		}
	})
}
