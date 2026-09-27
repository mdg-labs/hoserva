package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/pool"
)

func TestWriteStorageTarget_WritesEveryUnit(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	diskMountUnits := []string{"mnt-disk1.mount", "mnt-parity1.mount"}

	if err := g.WriteStorageTarget(ctx, diskMountUnits, "array status", 1, now); err != nil {
		t.Fatalf("WriteStorageTarget: %v", err)
	}

	readyWant := Header("array status", 1, now) + pool.StorageReadyUnit{}.Render()
	readyGot, err := os.ReadFile(filepath.Join(g.Root, storageTargetUnitDir+pool.StorageReadyUnitName))
	if err != nil {
		t.Fatalf("reading %s: %v", pool.StorageReadyUnitName, err)
	}
	if string(readyGot) != readyWant {
		t.Fatalf("%s mismatch:\ngot:\n%s\nwant:\n%s", pool.StorageReadyUnitName, readyGot, readyWant)
	}

	targetWant := Header("array status", 1, now) + pool.StorageTargetUnit{DiskMountUnits: diskMountUnits}.Render()
	targetGot, err := os.ReadFile(filepath.Join(g.Root, storageTargetUnitDir+pool.StorageTargetUnitName))
	if err != nil {
		t.Fatalf("reading %s: %v", pool.StorageTargetUnitName, err)
	}
	if string(targetGot) != targetWant {
		t.Fatalf("%s mismatch:\ngot:\n%s\nwant:\n%s", pool.StorageTargetUnitName, targetGot, targetWant)
	}

	for _, svc := range pool.DependentServiceUnits {
		dropIn := pool.ServiceDropIn{Unit: svc}
		want := Header("array status", 1, now) + dropIn.Render()
		got, err := os.ReadFile(filepath.Join(g.Root, dropIn.DropInPath()))
		if err != nil {
			t.Fatalf("reading %s drop-in: %v", svc, err)
		}
		if string(got) != want {
			t.Fatalf("%s drop-in mismatch:\ngot:\n%s\nwant:\n%s", svc, got, want)
		}
	}
}

// TestWriteStorageTarget_IdempotentAcrossReadinessAlone proves every file
// WriteStorageTarget writes is byte-identical whether disk.StorageGate is
// ready or not: readiness lives only in cmd/hoservad's own runtime flag
// (pool.StorageReadyFlagPath), never in this generated content, so a
// caller re-running this after nothing but readiness changed has nothing
// to reload a running hoserva-storage-ready.service over (#372's rejected
// attempt 1 rewrote ExecStart itself here and restarted the unit for
// exactly that case).
func TestWriteStorageTarget_IdempotentAcrossReadinessAlone(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

	if err := g.WriteStorageTarget(ctx, nil, "array status", 1, now); err != nil {
		t.Fatalf("WriteStorageTarget (revision 1): %v", err)
	}
	before, err := os.ReadFile(filepath.Join(g.Root, storageTargetUnitDir+pool.StorageReadyUnitName))
	if err != nil {
		t.Fatalf("reading %s: %v", pool.StorageReadyUnitName, err)
	}

	// A second call with the same revision and timestamp — standing in
	// for "readiness alone changed, disk topology did not" — must produce
	// the identical file: nothing here varies with readiness.
	if err := g.WriteStorageTarget(ctx, nil, "array status", 1, now); err != nil {
		t.Fatalf("WriteStorageTarget (repeat): %v", err)
	}
	after, err := os.ReadFile(filepath.Join(g.Root, storageTargetUnitDir+pool.StorageReadyUnitName))
	if err != nil {
		t.Fatalf("reading %s: %v", pool.StorageReadyUnitName, err)
	}
	if string(before) != string(after) {
		t.Fatalf("%s changed across an identical rewrite:\nbefore:\n%s\nafter:\n%s", pool.StorageReadyUnitName, before, after)
	}
}
