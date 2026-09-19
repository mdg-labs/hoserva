//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45),
// never on the host: it is built with `go test -tags lab -c` from the
// host (compiling touches no device) and the resulting binary is run
// with `docker compose exec -T lab <binary>` inside the lab container.
// It proves createArray's RunFunc (job.RunDiskFormat → disk.FormatPlan)
// formats only assigned loop devices, refuses a non-loop path before
// mkfs, and leaves an attached but unassigned loop untouched.

package job

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func labDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	return filepath.Join("/lab", id)
}

func createLoopImage(ctx context.Context, t *testing.T, r disk.Runner, lab, name string) string {
	t.Helper()

	imgDir := filepath.Join(lab, "img")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", imgDir, err)
	}
	img := filepath.Join(imgDir, name+".img")
	if err := os.Remove(img); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing stale %s: %v", img, err)
	}

	if _, err := r.Run(ctx, "truncate", "-s", "320M", img); err != nil {
		t.Fatalf("truncate %s: %v", img, err)
	}

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
		_, _ = r.Run(context.Background(), "losetup", "-d", dev)
	})
	return dev
}

func blkidType(ctx context.Context, r disk.Runner, dev string) string {
	out, _ := r.Run(ctx, "blkid", "-s", "TYPE", "-o", "value", dev)
	return strings.TrimSpace(string(out))
}

// TestLabCreateArray_RunFuncFormatsOnlyAssignedLoops is the create-array
// job's central safety-critical property, exercised against real mkfs
// and real loop devices: an attached but unassigned loop is not
// formatted, a non-loop path standing in for a real disk is refused
// before mkfs, and only the plan's own assigned loops are formatted.
func TestLabCreateArray_RunFuncFormatsOnlyAssignedLoops(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	exec := disk.CommandRunner{}
	provider := &disk.LinuxProvider{Lister: disk.NewLister(), Exec: exec}

	parity := createLoopImage(ctx, t, exec, lab, "create-array-parity")
	data := createLoopImage(ctx, t, exec, lab, "create-array-data")
	spare := createLoopImage(ctx, t, exec, lab, "create-array-spare")
	loopSize := int64(320 << 20)

	s := newTestScheduler(t)
	s.registry.Register(TypeDiskFormat, false, RunDiskFormat(provider, exec))

	good := DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: parity, Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: data, Filesystem: disk.XFS}},
		Sizes:  map[string]int64{parity: loopSize, data: loopSize},
	}
	good.Confirmation = good.Plan().Confirmation()

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, good))
	if err != nil {
		t.Fatalf("Submit(assigned loops): %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("assigned loops: status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if got := blkidType(ctx, exec, parity); got != "xfs" {
		t.Fatalf("parity %s: blkid TYPE = %q, want xfs", parity, got)
	}
	if got := blkidType(ctx, exec, data); got != "xfs" {
		t.Fatalf("data %s: blkid TYPE = %q, want xfs", data, got)
	}
	if got := blkidType(ctx, exec, spare); got != "" {
		t.Fatalf("spare %s gained a filesystem (%q)", spare, got)
	}

	bad := DiskFormatParams{
		Parity: []disk.AssignedDisk{{Device: spare, Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: "/dev/sda1", Filesystem: disk.XFS}},
		Sizes:  map[string]int64{spare: loopSize, "/dev/sda1": loopSize},
	}
	bad.Confirmation = bad.Plan().Confirmation()

	j, err = s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, bad))
	if err != nil {
		t.Fatalf("Submit(non-loop path): %v", err)
	}
	finished = await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("non-loop path: status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "not a loop device") && !strings.Contains(finished.ErrorMessage, "unmanaged") {
		t.Fatalf("non-loop path ErrorMessage = %q, want unmanaged-device refusal", finished.ErrorMessage)
	}
	if got := blkidType(ctx, exec, spare); got != "" {
		t.Fatalf("spare %s was formatted (%q) when the plan named /dev/sda1", spare, got)
	}
}
