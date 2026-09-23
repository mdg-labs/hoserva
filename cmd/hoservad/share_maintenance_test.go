package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
)

// noopChownFS is share.OSFS with Chown as a no-op — see
// TestShares_RefusedInMaintenanceModeAfterStopArray.
type noopChownFS struct{ share.OSFS }

func (noopChownFS) Chown(string, int, int) error { return nil }

// TestShares_RefusedInMaintenanceModeAfterStopArray is #333's acceptance
// criterion: after StopArray (Q70), createShare/updateShare/deleteShare
// must return 409 maintenance_mode and must not mkdir under a data-disk
// mountpoint — those directories sit on the root filesystem once the disks
// are unmounted, and the next array start would hide anything written
// there. Built the way main.go wires Handler.Shares + Handler.Array +
// Handler.Scheduler, so the refusal is reached through the same path
// hoservad serves.
func TestShares_RefusedInMaintenanceModeAfterStopArray(t *testing.T) {
	ctx, h, arrays, shares, disks, runner := newArrayTestEnv(t)

	root := t.TempDir()
	disk1 := filepath.Join(root, "disk1")
	assigned := []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", WWN: "wwn-p", Serial: "PARITY1", ByIDName: "wwn-wwn-p", Mountpoint: filepath.Join(root, "parity1")},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d", WWN: "wwn-d", Serial: "DATA1", ByIDName: "wwn-wwn-d", Mountpoint: disk1},
	}
	for _, d := range assigned {
		if err := os.MkdirAll(d.Mountpoint, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d.Mountpoint, err)
		}
	}
	if err := arrays.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "20G",
		CreatedAt:    time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC),
	}, assigned); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	presentMatchingDisks(disks, assigned)
	attachDaemonArray(t, ctx, h, arrays, shares, disks, runner)

	generator := cfggen.NewGenerator(filepath.Join(root, "etc"))
	shareService := newShareService(shares, arrays, generator, wiringTestMounter{}, nil)
	// Chown to ShareGID (100) fails when that group is absent (EINVAL as
	// root) or when the test process isn't in it (EPERM). Production
	// hoservad runs as root on Debian where `users` is GID 100; this
	// acceptance test is about the maintenance gate, not Q26 ownership,
	// so replace OSFS's Chown with a no-op the way internal/share's own
	// ownerRecordingFS does.
	shareService.FS = noopChownFS{}
	shareService.PostCommit = func(ctx context.Context) error {
		seq, err := newArraySequence(ctx, h.Scheduler, arrays, shares, disks, runner)
		if err != nil {
			return err
		}
		h.Array = seq
		return nil
	}
	h.Shares = shareService

	created, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{
		Name:      "media",
		CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly),
	})
	if err != nil {
		t.Fatalf("CreateShare(media) before stop: %v", err)
	}
	if created.Name != "media" {
		t.Fatalf("CreateShare(media) = %+v", created)
	}
	if _, err := os.Stat(filepath.Join(disk1, "media")); err != nil {
		t.Fatalf("CreateShare(media) did not create %s: %v", filepath.Join(disk1, "media"), err)
	}

	// Swap real mergerfs mounts for argv-recording fakes before Stop
	// (#207) — same pattern as TestNewArraySequence_SharesRejoinStopAndStart.
	if len(h.Array.ShareMounts) < 2 {
		t.Fatalf("ShareMounts = %d after Create(media), want at least 2", len(h.Array.ShareMounts))
	}
	realCatchAll, ok := h.Array.CatchAll.(pool.MountController)
	if !ok {
		t.Fatalf("CatchAll is %T, want pool.MountController", h.Array.CatchAll)
	}
	h.Array.CatchAll = arrayTestCatchAll{where: pool.CatchAllPath, argv: realCatchAll.Mnt.Argv(), runner: runner}
	for i, m := range h.Array.ShareMounts {
		sm, ok := m.(pool.MountController)
		if !ok {
			t.Fatalf("ShareMounts[%d] is %T, want pool.MountController", i, m)
		}
		h.Array.ShareMounts[i] = arrayTestCatchAll{where: sm.Where(), argv: sm.Mnt.Argv(), runner: runner}
	}

	if _, err := h.StopArray(ctx, confirmStop()); err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	if !h.Scheduler.InMaintenance() {
		t.Fatal("Scheduler.InMaintenance() = false after StopArray")
	}

	_, err = h.CreateShare(ctx, &apiv1.CreateShareRequest{
		Name:      "hidden",
		CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly),
	})
	status := handlerAPIError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "maintenance_mode" {
		t.Fatalf("CreateShare after StopArray = %+v, want 409 maintenance_mode", status)
	}
	if _, err := os.Stat(filepath.Join(disk1, "hidden")); !os.IsNotExist(err) {
		t.Fatalf("CreateShare(hidden) while stopped left %s (err=%v) — writes under a bare mountpoint are hidden on the next array start", filepath.Join(disk1, "hidden"), err)
	}
	if _, err := shares.Get(ctx, "hidden"); err == nil {
		t.Fatal("CreateShare(hidden) while stopped persisted a share row")
	}

	_, err = h.UpdateShare(ctx, &apiv1.UpdateShareRequest{
		CreatePolicy: apiv1.NewOptArrayCreatePolicy(apiv1.ArrayCreatePolicyMfs),
	}, apiv1.UpdateShareParams{Name: "media"})
	status = handlerAPIError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "maintenance_mode" {
		t.Fatalf("UpdateShare after StopArray = %+v, want 409 maintenance_mode", status)
	}

	err = h.DeleteShare(ctx, &apiv1.ConfirmShareRequest{Confirm: true}, apiv1.DeleteShareParams{Name: "media"})
	status = handlerAPIError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "maintenance_mode" {
		t.Fatalf("DeleteShare after StopArray = %+v, want 409 maintenance_mode", status)
	}
	if _, err := shares.Get(ctx, "media"); err != nil {
		t.Fatalf("DeleteShare while stopped removed the share row: %v", err)
	}
}
