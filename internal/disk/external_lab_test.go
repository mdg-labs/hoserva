//go:build lab

package disk

import (
	"context"
	"os"
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
	if got := blkidType(ctx, r, dev); got != "" {
		t.Fatalf("wrong confirmation formatted %s (%q)", dev, got)
	}

	if err := FormatExternal(ctx, linux, r, assigned, ExternalFormatPlan(assigned).Confirmation()); err != nil {
		t.Fatalf("FormatExternal: %v", err)
	}
	if got := blkidType(ctx, r, dev); got != "xfs" {
		t.Fatalf("blkid TYPE = %q, want xfs", got)
	}
	uuid := blkidUUID(ctx, r, dev)
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

func blkidUUID(ctx context.Context, r Runner, dev string) string {
	out, _ := r.Run(ctx, "blkid", "-s", "UUID", "-o", "value", dev)
	return strings.TrimSpace(string(out))
}
