//go:build lab

package disk

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type loopSpindownProvider struct {
	*LinuxProvider
}

func (p loopSpindownProvider) Spindown(ctx context.Context, dev string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func TestLabExternal_FormatMountEjectByUUID(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	linux := &LinuxProvider{Lister: NewLister(), Exec: r}
	provider := loopSpindownProvider{LinuxProvider: linux}
	mounter := DirectMounter{Runner: r}

	dev := createLoopImage(ctx, t, r, lab, "external-115")
	assigned := AssignedDisk{Device: dev, Filesystem: XFS}
	wrong := "yes"
	if err := FormatExternal(ctx, linux, r, assigned, wrong); err == nil {
		t.Fatal("FormatExternal(wrong confirmation): expected an error")
	}
	if got := blkidType(t, ctx, r, dev); got != "" {
		t.Fatalf("wrong confirmation formatted %s (%q)", dev, got)
	}

	if err := FormatExternal(ctx, linux, r, assigned, ExternalFormatPlan(assigned).Confirmation()); err != nil {
		t.Fatalf("FormatExternal: %v", err)
	}
	if got := blkidType(t, ctx, r, dev); got != "xfs" {
		t.Fatalf("blkid TYPE = %q, want xfs", got)
	}
	uuid := blkidUUID(t, ctx, r, dev)
	if uuid == "" {
		t.Fatal("formatted disk has no filesystem UUID")
	}

	unit, err := ExternalMountUnit("ext115", uuid, XFS)
	if err != nil {
		t.Fatalf("ExternalMountUnit: %v", err)
	}
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), "umount", unit.Where)
	})
	if err := MountExternal(ctx, mounter, unit); err != nil {
		t.Fatalf("MountExternal: %v", err)
	}
	mounted, err := IsMountpoint(unit.Where)
	if err != nil || !mounted {
		t.Fatalf("IsMountpoint(%s) = (%v, %v), want mounted", unit.Where, mounted, err)
	}
	if err := os.WriteFile(filepath.Join(unit.Where, "container-path.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("writing through external mount (container path): %v", err)
	}

	if err := EjectExternal(ctx, mounter, provider, unit, dev); err != nil {
		t.Fatalf("EjectExternal: %v", err)
	}
	mounted, err = IsMountpoint(unit.Where)
	if err != nil || mounted {
		t.Fatalf("after eject IsMountpoint(%s) = (%v, %v), want unmounted", unit.Where, mounted, err)
	}
}

// blkidUUID reads dev's filesystem UUID by direct probe (`blkid -p`),
// for the same reason blkidType in format_lab_test.go does: a plain
// `blkid` can return a cached result from a device that previously held
// this loop minor (finding 1, #398). blkid -p exits 2 both for "no
// signature found" and for a device it could not even open (confirmed
// against real blkid in this lab, same as blkidType), so this checks dev
// exists before ever invoking blkid, and still fails on any non-exit-2
// blkid result (#400).
func blkidUUID(t testing.TB, ctx context.Context, r Runner, dev string) string {
	t.Helper()
	if _, err := os.Stat(dev); err != nil {
		t.Fatalf("blkid -p -s UUID -o value %s: device not accessible: %v", dev, err)
	}
	out, err := r.Run(ctx, "blkid", "-p", "-s", "UUID", "-o", "value", dev)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return ""
		}
		t.Fatalf("blkid -p -s UUID -o value %s: %v (output %q)", dev, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestLabBlkidUUID_FailsOnNonExitTwoError is this issue's own proving
// test (#400) for blkidUUID, mirroring
// TestLabBlkidType_FailsOnNonExitTwoError in format_lab_test.go: a
// nonexistent device path is a genuine "can't open" failure, never
// exit 2, and must fail the caller rather than read back "".
func TestLabBlkidUUID_FailsOnNonExitTwoError(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	nonexistent := filepath.Join(lab, "no-such-device-400")

	rec := &recordingTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		blkidUUID(rec, ctx, r, nonexistent)
	}()
	<-done
	if !rec.failed {
		t.Fatal("blkidUUID(nonexistent path) returned instead of failing — a swallowed non-exit-2 error would pass a blank-device assertion vacuously")
	}
	t.Logf("blkidUUID correctly failed: %s", rec.message)
}
