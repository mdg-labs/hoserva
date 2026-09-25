package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
)

// newLiveDiskReplaceShareUsageEnv builds, on top of newParityRegistrationEnv,
// the two consumers #362 covers exactly the way main.go builds them —
// currentParityEngine{handler: handler} for the disk-replace/upgrade jobs'
// Parity dependency, and parity.NewUsageStore(db) for share.Service's
// Usages — both before any array exists, so neither can be a startup
// snapshot of a nil parityEngine.
func newLiveDiskReplaceShareUsageEnv(t *testing.T) (context.Context, *parityRegistrationEnv, currentParityEngine, *share.Service, *parity.UsageStore) {
	t.Helper()
	ctx, env := newParityRegistrationEnv(t)

	replaceParityEngine := currentParityEngine{handler: env.handler}
	usageStore := parity.NewUsageStore(env.db)
	shareService := newShareService(env.shareStore, env.handler.ArrayStore, cfggen.NewGenerator(env.configRoot), nil, usageStore)

	return ctx, env, replaceParityEngine, shareService, usageStore
}

// TestDiskReplaceParityAndShareUsage_LiveAfterArrayCreation is #362's own
// regression: a daemon started with no array wired the disk-replace/
// upgrade jobs' Parity dependency and share.Service's usage reader from a
// startup-time snapshot that stayed nil/unavailable after a live `POST
// /disks/array` (#265's own live-set mechanism only covered
// TypeSync/TypeScrub/.../Handler's own parity-derived fields). This
// builds both consumers the way main.go does, drives the same live array
// creation TestParityRegistrar_WiresJobTypesAndHandlerFieldsAfterLiveArrayCreation
// does, and proves both are reachable afterward, with no restart:
// replaceParityEngine no longer reports errNoParityEngine, and
// share.Service.Get reflects a completed sync's write immediately.
func TestDiskReplaceParityAndShareUsage_LiveAfterArrayCreation(t *testing.T) {
	ctx, env, replaceParityEngine, shareService, usageStore := newLiveDiskReplaceShareUsageEnv(t)
	h := env.handler

	env.provider.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, WWN: "wwn-parity", Serial: "PARITY1", ByIDName: "wwn-wwn-parity"})
	env.provider.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB, Serial: "DATA1"})
	env.provider.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB, Serial: "DATA2"})
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/disk/by-id/wwn-wwn-parity"}, []byte("uuid-parity1\n"), nil)
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdb"}, []byte("uuid-disk1\n"), nil)
	env.runner.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdc"}, []byte("uuid-disk2\n"), nil)

	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := env.shareStore.Insert(ctx, store.Share{Name: "media", CacheMode: "array-only", CreatePolicy: "mfs", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("inserting share: %v", err)
	}

	// Before the array exists: the disk-replace/upgrade Parity source
	// reports the honest "no array yet" error rather than resolving a
	// nil engine, and the share's usage is honestly unsynced. engine()
	// (not Status/Diff/...) is called directly so this never shells out
	// to a real snapraid binary, which this dev environment has none of.
	if _, err := replaceParityEngine.engine(); !errors.Is(err, errNoParityEngine) {
		t.Fatalf("replaceParityEngine.engine() before array creation = %v, want errNoParityEngine", err)
	}
	sh, err := shareService.Get(ctx, "media")
	if err != nil {
		t.Fatalf("Get before array creation: %v", err)
	}
	if sh.Usage.Synced {
		t.Fatal("share usage reports synced before any sync has ever run")
	}

	j, err := h.CreateArray(ctx, liveArrayCreateReq("/dev/sda", "/dev/sdb", "/dev/sdc"))
	if err != nil {
		t.Fatalf("CreateArray: %v", err)
	}
	finished, err := h.Scheduler.Await(ctx, j.ID.String())
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("create-array job status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	// After the live creation, no restart: the disk-replace/upgrade
	// Parity source resolves the engine parityReg.ensure just wired.
	if _, err := replaceParityEngine.engine(); err != nil {
		t.Fatalf("replaceParityEngine.engine() after a live array creation: %v — d.Parity would still fail disk replace/upgrade until a restart (#362)", err)
	}

	// share.Service's usage reader reflects a completed sync's write
	// immediately — no re-wiring step, since it was never a captured
	// nil to begin with. Writing directly to usageStore stands in for
	// the sync job's own ComputeShareUsage (out of #362's own scope).
	if err := usageStore.Replace(ctx, []parity.ShareUsage{{Share: "media", Disk: "/mnt/disk1", Bytes: 12345}}, time.Now()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	sh, err = shareService.Get(ctx, "media")
	if err != nil {
		t.Fatalf("Get after a completed sync: %v", err)
	}
	if !sh.Usage.Synced || sh.Usage.TotalBytes != 12345 {
		t.Fatalf("share usage after a completed sync = %+v, want Synced with TotalBytes=12345 — listShares/getShare would still show no usage until a restart (#362)", sh.Usage)
	}
}

// TestDiskReplaceParityAndShareUsage_SafeForConcurrentReads proves
// replaceParityEngine.engine() and shareService.Get, read from goroutines
// standing in for concurrent HTTP requests, race-detect clean
// (go test -race) against a concurrent ensure call standing in for the
// live array-creation job's own ArrayReady hook — the same concurrency
// shape TestParityRegistrar_Ensure_SafeForConcurrentCurrentParityReads
// already proves for Handler.CurrentParity/diffGuardHolder.
func TestDiskReplaceParityAndShareUsage_SafeForConcurrentReads(t *testing.T) {
	ctx, env, replaceParityEngine, shareService, usageStore := newLiveDiskReplaceShareUsageEnv(t)

	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := env.shareStore.Insert(ctx, store.Share{Name: "media", CacheMode: "array-only", CreatePolicy: "mfs", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("inserting share: %v", err)
	}
	if err := os.WriteFile(filepath.Join(env.configRoot, snapraidConfRelPath), []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("writing placeholder snapraid.conf: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					replaceParityEngine.engine()   //nolint:errcheck
					shareService.Get(ctx, "media") //nolint:errcheck
				}
			}
		}()
	}

	if err := usageStore.Replace(ctx, []parity.ShareUsage{{Share: "media", Disk: "/mnt/disk1", Bytes: 999}}, time.Now()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if err := env.parityReg.ensure(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	close(stop)
	wg.Wait()

	if _, err := replaceParityEngine.engine(); err != nil {
		t.Fatalf("replaceParityEngine.engine() after ensure: %v", err)
	}
}
