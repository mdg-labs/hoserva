//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern mount_lab_test.go and add_disk_lab_test.go use. It proves
// StatfsSpaceStatter reports real, changing free-space figures against
// the standing lab's own data disks — confirming this package's free-
// space accounting comes from the kernel's own statfs(2) counters, never
// a directory walk, against a real filesystem rather than only a fake
// one (doc 09 §5).

package pool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestLabComputePoolSpace_ReflectsRealUsage writes a known-size file to
// one of the lab's real data disks and confirms ComputePoolSpace's own
// figures — read entirely through statfs(2), with this test never once
// listing a directory itself — shrink by roughly that amount, and that
// the disk crosses NearMinFreeSpace once a large enough minfreespace
// value is configured against it.
func TestLabComputePoolSpace_ReflectsRealUsage(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()

	dataDisks := []string{
		filepath.Join(lab, "mnt", "disk1"),
		filepath.Join(lab, "mnt", "disk2"),
		filepath.Join(lab, "mnt", "disk3"),
	}
	for _, d := range dataDisks {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("data disk %s not present — expected create-array.sh to have mounted it: %v", d, err)
		}
	}

	statter := StatfsSpaceStatter{}

	// minfreespace is derived from disk1's own free space right now,
	// comfortably below it — rather than a hardcoded size — so the test
	// holds regardless of the lab image's own filesystem overhead
	// (create-array.sh's DATA_SIZE, xfs reserved blocks, etc.): disk1
	// starts under the floor, then crosses it once the probe file is
	// written.
	const written = 64 * 1 << 20 // 64 MiB
	initial, err := statter.StatSpace(ctx, dataDisks[0])
	if err != nil {
		t.Fatalf("StatSpace (initial): %v", err)
	}
	if initial.FreeBytes < written*2 {
		t.Fatalf("disk1 has only %d bytes free, want at least %d for this test's own probe write to be a small fraction of it", initial.FreeBytes, written*2)
	}
	minFreeSpace := fmt.Sprintf("%d", initial.FreeBytes-written/2)

	before, err := ComputePoolSpace(ctx, statter, dataDisks, minFreeSpace)
	if err != nil {
		t.Fatalf("ComputePoolSpace (before): %v", err)
	}
	if before.PoolFreeBytes <= 0 {
		t.Fatalf("PoolFreeBytes (before) = %d, want > 0 on a fresh lab disk", before.PoolFreeBytes)
	}
	for _, d := range before.Disks {
		if d.Path == dataDisks[0] && d.NearMinFreeSpace {
			t.Fatalf("disk1 already NearMinFreeSpace before writing anything: %+v", d)
		}
	}

	probe := filepath.Join(dataDisks[0], "space-probe.bin")
	if err := os.WriteFile(probe, make([]byte, written), 0o644); err != nil {
		t.Fatalf("writing probe file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(probe) })

	after, err := ComputePoolSpace(ctx, statter, dataDisks, minFreeSpace)
	if err != nil {
		t.Fatalf("ComputePoolSpace (after): %v", err)
	}

	shrink := before.PoolFreeBytes - after.PoolFreeBytes
	if shrink < written/2 {
		t.Fatalf("pool free space shrank by %d bytes after writing %d, want at least half of that reflected", shrink, written)
	}

	// disk1 has now crossed the minfreespace floor this test derived from
	// its own pre-write free space — proving NearMinFreeSpace reacts to a
	// real write, not only to FakeSpaceStatter's scripted numbers.
	foundConstrained := false
	for _, d := range after.Disks {
		if d.Path == dataDisks[0] && d.NearMinFreeSpace {
			foundConstrained = true
		}
	}
	if !foundConstrained {
		t.Fatalf("disk1 not flagged NearMinFreeSpace after writing %d bytes toward a %s-byte floor: %+v", written, minFreeSpace, after.Disks[0])
	}
}
