//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the same
// pattern internal/pool's own mount_lab_test.go uses. It proves #409's
// mount check end to end against a real mergerfs catch-all — never a fake
// PoolMounted — genuinely mounted, then genuinely unmounted.

package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/pool"

	_ "modernc.org/sqlite"
)

func labDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	return filepath.Join("/lab", id)
}

// TestLabService_RunSkipsPoolDestinationOnceGenuinelyUnmounted is #409's
// own lab acceptance test: with a real mergerfs catch-all mounted, Run
// writes the archive through it onto a real data disk; once that mount is
// genuinely torn down, Run writes nothing to the same path and still
// succeeds through its own separate, boot-style destination —
// pool.IsMountedConfirmed, the production default (Service.PoolMounted
// left unset), is what tells the two states apart, not a test fake.
func TestLabService_RunSkipsPoolDestinationOnceGenuinelyUnmounted(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	mounter := pool.Mounter{Runner: disk.CommandRunner{}}

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

	opts := pool.Options{MinFreeSpace: "50M", Responsiveness: pool.Responsive}
	catchAllWhere := filepath.Join(lab, "mnt", "pool-backup-test")
	if err := os.MkdirAll(catchAllWhere, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", catchAllWhere, err)
	}
	catchAll, err := pool.CatchAllMount(dataDisks, opts)
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}
	catchAll.Where = catchAllWhere

	t.Cleanup(func() {
		_ = mounter.Unmount(context.Background(), catchAllWhere)
	})

	if err := mounter.Mount(ctx, catchAll); err != nil {
		t.Fatalf("mounting catch-all: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	bootDest := filepath.Join(t.TempDir(), "boot-backups")
	poolDest := filepath.Join(catchAllWhere, "hoserva-backups")

	first := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	svc := &Service{
		DB: db,
		Destinations: []Destination{
			{ID: "boot", Path: bootDest, Enabled: true, Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}},
			{ID: "pool", Path: poolDest, Enabled: true, Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}},
		},
		Hostname: "lab-host",
		Version:  "0.0.0-lab",
		Now:      func() time.Time { return first },
		PoolRoot: catchAllWhere,
	}

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run while the pool is genuinely mounted: %v", err)
	}
	firstName := archiveName(first, ReasonNone, 0)
	if _, err := os.Stat(filepath.Join(poolDest, firstName)); err != nil {
		t.Fatalf("archive missing from the pool destination while genuinely mounted: %v", err)
	}
	onArray := false
	for _, d := range dataDisks {
		if _, err := os.Stat(filepath.Join(d, "hoserva-backups", firstName)); err == nil {
			onArray = true
		}
	}
	if !onArray {
		t.Fatal("pool archive did not land on any data disk through the mounted catch-all")
	}

	if err := mounter.Unmount(ctx, catchAllWhere); err != nil {
		t.Fatalf("unmounting catch-all: %v", err)
	}

	second := first.Add(time.Hour)
	svc.Now = func() time.Time { return second }
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run once the pool is genuinely unmounted: %v", err)
	}
	secondName := archiveName(second, ReasonNone, 0)
	if _, err := os.Stat(filepath.Join(bootDest, secondName)); err != nil {
		t.Fatalf("boot archive missing after the pool was unmounted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(poolDest, secondName)); !os.IsNotExist(err) {
		t.Fatalf("pool destination received a write while genuinely unmounted (#409): stat error = %v", err)
	}
	for _, d := range dataDisks {
		if _, err := os.Stat(filepath.Join(d, "hoserva-backups", secondName)); err == nil {
			t.Fatalf("second archive landed on data disk %s despite the catch-all being genuinely unmounted (#409)", d)
		}
	}
}
