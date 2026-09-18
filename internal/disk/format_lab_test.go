//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45),
// never on the host: it is built with `go test -tags lab -c` from the
// host (compiling touches no device) and the resulting binary is run
// with `docker compose exec -T lab <binary>` inside the lab container,
// the same way `make lab-up` runs create-array.sh (CLAUDE.md: no
// mkfs/losetup/mount on the host, against anything). It proves
// FormatAssigned and LinuxProvider.Format work against a real loop
// device and a real mkfs.xfs, and that FormatAssigned refuses a real,
// attached device that was never assigned to the plan driving it —
// before any device is touched.

package disk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func labDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	return filepath.Join("/lab", id)
}

// createLoopImage truncates a small sparse image under lab's own img/
// directory and attaches it to a loop device, mirroring lib.sh's own
// lab_assert_own_loop safety check: the resulting device must actually
// be the loop device losetup just attached to the image this call
// created, never trusted on the strength of a returned path alone.
func createLoopImage(ctx context.Context, t *testing.T, r Runner, lab, name string) string {
	t.Helper()

	imgDir := filepath.Join(lab, "img")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", imgDir, err)
	}
	img := filepath.Join(imgDir, name+".img")
	if err := os.Remove(img); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing stale %s: %v", img, err)
	}

	// mkfs.xfs refuses anything under 300M ("Filesystem must be larger
	// than 300MB"), confirmed against the real tool in this lab.
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

func blkidType(ctx context.Context, r Runner, dev string) string {
	out, _ := r.Run(ctx, "blkid", "-s", "TYPE", "-o", "value", dev)
	return strings.TrimSpace(string(out))
}

// TestLabFormat_RefusesUnassignedDevicesThenFormatsTheAssignedOne is
// this issue's central safety-critical property, exercised against a
// real filesystem tool and real loop devices rather than a fake: a
// device that is attached and real, but was never explicitly assigned a
// role in the plan driving the operation, is refused before any mkfs
// runs — and a device path that was never attached at all (standing in
// for a real, non-loop disk a bug might otherwise reach) is refused the
// same way. Only the plan's own device is ever formatted.
func TestLabFormat_RefusesUnassignedDevicesThenFormatsTheAssignedOne(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	provider := &LinuxProvider{Lister: NewLister(), Exec: r}

	assigned := createLoopImage(ctx, t, r, lab, "format-lab-assigned")
	spare := createLoopImage(ctx, t, r, lab, "format-lab-spare")

	plan := TopologyPlan{Data: []AssignedDisk{{Device: assigned, Filesystem: XFS}}}

	if err := FormatAssigned(ctx, provider, plan, spare, XFS); err == nil {
		t.Fatal("FormatAssigned(spare, real but unassigned): got nil error")
	}
	if got := blkidType(ctx, r, spare); got != "" {
		t.Fatalf("spare device %s already has a filesystem (%q) before any assigned format ran", spare, got)
	}

	if err := FormatAssigned(ctx, provider, plan, "/dev/sda1", XFS); err == nil {
		t.Fatal("FormatAssigned(/dev/sda1, never attached): got nil error")
	}

	if err := FormatAssigned(ctx, provider, plan, assigned, XFS); err != nil {
		t.Fatalf("FormatAssigned(assigned): %v", err)
	}
	if got := blkidType(ctx, r, assigned); got != "xfs" {
		t.Fatalf("assigned device %s: blkid TYPE = %q, want xfs", assigned, got)
	}
	if got := blkidType(ctx, r, spare); got != "" {
		t.Fatalf("spare device %s gained a filesystem (%q) after the assigned-only format ran", spare, got)
	}
}
