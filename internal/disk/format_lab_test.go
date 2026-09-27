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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// blkidType reads dev's filesystem type by direct probe (`blkid -p`),
// bypassing libblkid's own cache: a loop device just detached by an
// earlier test in this lab run can be reattached under the same minor
// number within this suite's own runtime (the host's udev holds a
// detach open briefly, confirmed in this lab), and a plain `blkid`
// without `-p` returns that stale cache entry rather than reprobing the
// device now in front of it (finding 1, #398). blkid -p exits 2 both for
// "no signature found" (the genuine blank-device case) and for a device
// it could not even open (confirmed against real blkid, util-linux
// 2.41.5, in this lab: a nonexistent path, an unreadable one and a truly
// blank device all exit 2 alike), so exit code alone cannot tell a blank
// device from a wrong path; this checks dev exists before ever invoking
// blkid, and still fails on any non-exit-2 blkid result, so a refused
// format's blank-device assertion cannot pass vacuously (#400).
func blkidType(t testing.TB, ctx context.Context, r Runner, dev string) string {
	t.Helper()
	if _, err := os.Stat(dev); err != nil {
		t.Fatalf("blkid -p -s TYPE -o value %s: device not accessible: %v", dev, err)
	}
	out, err := r.Run(ctx, "blkid", "-p", "-s", "TYPE", "-o", "value", dev)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return ""
		}
		t.Fatalf("blkid -p -s TYPE -o value %s: %v (output %q)", dev, err, out)
	}
	return strings.TrimSpace(string(out))
}

// recordingTB is a minimal testing.TB whose Fatalf records a failure
// instead of stopping this test binary's own run, so blkidType's and
// blkidUUID's fatal path (#400) can be observed as a value rather than
// only by process exit. Embedding the nil testing.TB satisfies the
// interface's unexported method without implementing every method:
// blkidType and blkidUUID call only Helper and Fatalf, so every other
// method staying nil is unreachable here.
type recordingTB struct {
	testing.TB
	failed  bool
	message string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failed = true
	r.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
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
	if got := blkidType(t, ctx, r, spare); got != "" {
		t.Fatalf("spare device %s already has a filesystem (%q) before any assigned format ran", spare, got)
	}

	if err := FormatAssigned(ctx, provider, plan, "/dev/sda1", XFS); err == nil {
		t.Fatal("FormatAssigned(/dev/sda1, never attached): got nil error")
	}

	if err := FormatAssigned(ctx, provider, plan, assigned, XFS); err != nil {
		t.Fatalf("FormatAssigned(assigned): %v", err)
	}
	if got := blkidType(t, ctx, r, assigned); got != "xfs" {
		t.Fatalf("assigned device %s: blkid TYPE = %q, want xfs", assigned, got)
	}
	if got := blkidType(t, ctx, r, spare); got != "" {
		t.Fatalf("spare device %s gained a filesystem (%q) after the assigned-only format ran", spare, got)
	}
}

// TestLabBlkidType_FailsOnNonExitTwoError is this issue's own proving
// test (#400): blkid -p exits 2 only for "no signature found", the
// genuine blank-device case; any other failure — a wrong device path, a
// missing binary, a permission error — must fail the caller rather than
// read back "" the same way, or a refused format's own blank-device
// assertion would pass vacuously. Run against real blkid in this lab
// container: a nonexistent device path is a genuine "can't open"
// failure, never exit 2.
func TestLabBlkidType_FailsOnNonExitTwoError(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	nonexistent := filepath.Join(lab, "no-such-device-400")

	rec := &recordingTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		blkidType(rec, ctx, r, nonexistent)
	}()
	<-done
	if !rec.failed {
		t.Fatal("blkidType(nonexistent path) returned instead of failing — a swallowed non-exit-2 error would pass a blank-device assertion vacuously")
	}
	t.Logf("blkidType correctly failed: %s", rec.message)
}
