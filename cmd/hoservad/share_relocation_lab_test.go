//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container. It is
// this issue's own lab acceptance criterion (#245): a real
// job.Scheduler, registered exactly the way main.go registers
// TypeShareRelocation, driving shareRelocationShareFromStore and
// shareRelocationSyncFunc against a real store.ArrayStore/ShareStore and
// a real parity.SnapraidEngine — proving the wiring reaches an actual
// `snapraid sync`, not just the fakes internal/job's own
// share_relocation_run_test.go scripts.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func shareRelocLabDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	return filepath.Join("/lab", id)
}

// shareRelocLabEngine is internal/parity's own labEngineWithMounts
// (lifecycle_lab_test.go) and internal/cache's relocateLabEngineWithMounts
// (relocate_lab_test.go), reproduced here for the same reason both of
// those reproduce it themselves: an unexported helper in a different
// package this file cannot import. filesetName keeps this file's own
// parity/content files apart from any other package's lab tests sharing
// the same standing lab.
func shareRelocLabEngine(t *testing.T, lab, workDirName, filesetName string, mounts []string, guard parity.GuardConfig) *parity.SnapraidEngine {
	t.Helper()
	parityDir := filepath.Join(lab, "mnt/parity1")
	cache := filepath.Join(lab, "mnt/cache")

	var conf strings.Builder
	fmt.Fprintf(&conf, "parity %s\n", filepath.Join(parityDir, filesetName+".parity"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(parityDir, filesetName+".content"))
	fmt.Fprintf(&conf, "content %s\n", filepath.Join(cache, filesetName+".content"))
	for i, m := range mounts {
		fmt.Fprintf(&conf, "data d%d %s\n", i+1, m+"/")
	}

	workDir := filepath.Join(lab, workDirName)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", workDir, err)
	}
	confPath := filepath.Join(workDir, "snapraid.conf")
	if err := os.WriteFile(confPath, []byte(conf.String()), 0o644); err != nil {
		t.Fatalf("writing %s: %v", confPath, err)
	}

	return &parity.SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(workDir, "logs"),
		Runner:   parity.CommandRunner{},
		Guard:    parity.Guard{Config: guard},
	}
}

func shareRelocLabDrain(t *testing.T, ch <-chan parity.Progress) parity.Progress {
	t.Helper()
	var last parity.Progress
	for p := range ch {
		last = p
	}
	return last
}

func shareRelocLabSyncOnce(t *testing.T, ctx context.Context, engine *parity.SnapraidEngine) {
	t.Helper()
	ch, err := engine.Sync(ctx, parity.SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	final := shareRelocLabDrain(t, ch)
	if final.Err != nil {
		t.Fatalf("Sync failed: %v", final.Err)
	}
}

func shareRelocLabWriteFile(t *testing.T, path string, sizeBytes int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	f, err := os.Open("/dev/urandom")
	if err != nil {
		t.Fatalf("open /dev/urandom: %v", err)
	}
	defer func() { _ = f.Close() }()
	data := make([]byte, sizeBytes)
	if _, err := f.Read(data); err != nil {
		t.Fatalf("reading random data: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// shareRelocLabStores builds a real, migrated SQLite database and inserts
// an array topology (settings.MinFreeSpace generous so it never blocks
// this test's small files) plus one share, using the same
// store.ArrayStore/ShareStore shareRelocationShareFromStore reads in
// production main.go.
func shareRelocLabStores(t *testing.T, cachePath string, dataMounts []string, shareName string) (*store.ShareStore, *store.ArrayStore) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-share-relocation-lab.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	arrays := store.NewArrayStore(db)
	disks := []store.ArrayDisk{{
		Role: store.ArrayRoleCache, RoleIndex: 1, Mountpoint: cachePath,
		Device: "/dev/loop-lab-cache", Filesystem: "xfs", FSUUID: "lab-cache", WWN: "lab-cache", Serial: "LABCACHE", ByIDName: "lab-cache",
	}}
	for i, m := range dataMounts {
		label := fmt.Sprintf("lab-disk%d", i+1)
		disks = append(disks, store.ArrayDisk{
			Role: store.ArrayRoleData, RoleIndex: i + 1, Mountpoint: m,
			Device: "/dev/" + label, Filesystem: "xfs", FSUUID: label, WWN: label, Serial: strings.ToUpper(label), ByIDName: label,
		})
	}
	now := time.Now().UTC()
	if err := arrays.PutArray(context.Background(), store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "1M", CreatedAt: now}, disks); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	shares := store.NewShareStore(db)
	if err := shares.Insert(context.Background(), store.Share{
		Name:         shareName,
		CacheMode:    "cache-then-move",
		CreatePolicy: "mfs",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("inserting share %s: %v", shareName, err)
	}

	return shares, arrays
}

func shareRelocLabScheduler(t *testing.T, registry *job.Registry) *job.Scheduler {
	t.Helper()
	return newRegistryTestScheduler(t, registry)
}

// TestLabShareRelocation_ToCache_RunsThroughRealRegistryAndSync proves
// the exact registration main.go performs — registry.Register(job.
// TypeShareRelocation, true, job.RunShareRelocation(job.
// ShareRelocationDeps{Share: shareRelocationShareFromStore(...), Sync:
// shareRelocationSyncFunc(parityEngine)})) — reaches a real snapraid
// sync end to end: a file written on a real data disk is copied to a
// real cache mount, a real `snapraid sync` runs, and the array original
// is deleted only after that sync succeeds (Q14).
func TestLabShareRelocation_ToCache_RunsThroughRealRegistryAndSync(t *testing.T) {
	lab := shareRelocLabDir(t)
	ctx := context.Background()

	dataMounts := []string{filepath.Join(lab, "mnt/disk1"), filepath.Join(lab, "mnt/disk2"), filepath.Join(lab, "mnt/disk3")}
	cacheRoot := filepath.Join(lab, "mnt/cache")
	shareName := "p245share"

	engine := shareRelocLabEngine(t, lab, "p245-relocate-test", "p245-relocate", dataMounts, parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100})

	shareDir := filepath.Join(dataMounts[0], shareName)
	src := filepath.Join(shareDir, "file.bin")
	shareRelocLabWriteFile(t, src, 300_000)

	// A second, unrelated file that stays on disk1 throughout — without
	// it, relocating file.bin away would drop disk1 to zero files and
	// trip the guard's own separate "disk went missing" rule
	// (TriggerZeroFiles), which is not what this test is about.
	stableOnDisk1 := filepath.Join(dataMounts[0], "other", "stays.bin")
	shareRelocLabWriteFile(t, stableOnDisk1, 20_000)

	shareRelocLabSyncOnce(t, ctx, engine)

	shares, arrays := shareRelocLabStores(t, cacheRoot, dataMounts, shareName)

	registry := job.NewRegistry()
	registry.Register(job.TypeShareRelocation, true, job.RunShareRelocation(job.ShareRelocationDeps{
		Share: shareRelocationShareFromStore(shares, arrays),
		Sync:  shareRelocationSyncFunc(engine),
	}))
	scheduler := shareRelocLabScheduler(t, registry)

	params, err := json.Marshal(job.ShareRelocationParams{Share: shareName, To: "cache"})
	if err != nil {
		t.Fatalf("marshaling ShareRelocationParams: %v", err)
	}
	j, err := scheduler.Submit(ctx, job.TypeShareRelocation, nil, params)
	if err != nil {
		t.Fatalf("Submit(TypeShareRelocation): %v", err)
	}
	finished, err := scheduler.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("array original should be gone after relocation: err=%v", err)
	}
	cached, err := os.ReadFile(filepath.Join(cacheRoot, shareName, "file.bin"))
	if err != nil {
		t.Fatalf("reading relocated cache file: %v", err)
	}
	if len(cached) != 300_000 {
		t.Fatalf("relocated cache file size = %d, want 300000", len(cached))
	}

	diff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after relocation: %v", err)
	}
	if diff.Added != 0 || diff.Removed != 0 || diff.Updated != 0 {
		t.Fatalf("Diff after relocation reported pending changes: %+v, want none — the job's own sync must already have covered them", diff)
	}
}

// TestLabShareRelocation_ToCache_GuardBlocked_LeavesArrayUntouched proves
// the real parityEngine.Sync adapter this issue wires is gated by the
// same threshold guard as any other sync — no new, ungated path from a
// share-relocation job to snapraid sync. RelocateToCache's own two-phase
// order (Q14) runs its first guarded sync *before* deleting anything
// (doc 09 §2: "a sync runs before the originals are removed"), against
// whatever the array's real diff already shows at that point — so this
// test trips the guard with real, unrelated drift (two files removed
// straight from a data disk, outside the share being relocated, after
// the baseline sync) rather than the relocation's own not-yet-run
// delete, and confirms that real drift alone is enough to block the
// relocation's own pre-delete sync before anything of the relocated
// share is touched — exactly as internal/job's own fake-engine test
// (TestRunShareRelocation_ToCache_GuardBlocked_LeavesArrayUntouched)
// already proves against a fake guard block.
func TestLabShareRelocation_ToCache_GuardBlocked_LeavesArrayUntouched(t *testing.T) {
	lab := shareRelocLabDir(t)
	ctx := context.Background()

	dataMounts := []string{filepath.Join(lab, "mnt/disk1"), filepath.Join(lab, "mnt/disk2"), filepath.Join(lab, "mnt/disk3")}
	cacheRoot := filepath.Join(lab, "mnt/cache")
	shareName := "p245guardshare"

	// RemovedFilesMax: 1 trips as soon as more than one file's removal
	// shows up in a real diff — real files, a real diff, no scripted
	// fake.
	engine := shareRelocLabEngine(t, lab, "p245-relocate-guard-test", "p245-relocate-guard", dataMounts, parity.GuardConfig{RemovedFilesMax: 1, RemovedUpdatedPercent: 100})

	shareDir := filepath.Join(dataMounts[0], shareName)
	src1 := filepath.Join(shareDir, "one.bin")
	src2 := filepath.Join(shareDir, "two.bin")
	shareRelocLabWriteFile(t, src1, 100_000)
	shareRelocLabWriteFile(t, src2, 100_000)

	// Two unrelated files on a different data disk, outside the share
	// being relocated and outside disk1's own zero-files exposure below.
	otherA := filepath.Join(dataMounts[1], "other", "a.bin")
	otherB := filepath.Join(dataMounts[1], "other", "b.bin")
	otherStable := filepath.Join(dataMounts[1], "other", "stays.bin")
	shareRelocLabWriteFile(t, otherA, 10_000)
	shareRelocLabWriteFile(t, otherB, 10_000)
	shareRelocLabWriteFile(t, otherStable, 10_000)

	shareRelocLabSyncOnce(t, ctx, engine)

	// Real, unrelated drift: two files genuinely gone from disk2 before
	// this relocation is ever submitted — disk2 still has otherStable so
	// this is a count trip (TriggerRemovedCount), not a zero-files one.
	if err := os.Remove(otherA); err != nil {
		t.Fatalf("removing %s: %v", otherA, err)
	}
	if err := os.Remove(otherB); err != nil {
		t.Fatalf("removing %s: %v", otherB, err)
	}

	shares, arrays := shareRelocLabStores(t, cacheRoot, dataMounts, shareName)

	registry := job.NewRegistry()
	registry.Register(job.TypeShareRelocation, true, job.RunShareRelocation(job.ShareRelocationDeps{
		Share: shareRelocationShareFromStore(shares, arrays),
		Sync:  shareRelocationSyncFunc(engine),
	}))
	scheduler := shareRelocLabScheduler(t, registry)

	params, err := json.Marshal(job.ShareRelocationParams{Share: shareName, To: "cache"})
	if err != nil {
		t.Fatalf("marshaling ShareRelocationParams: %v", err)
	}
	j, err := scheduler.Submit(ctx, job.TypeShareRelocation, nil, params)
	if err != nil {
		t.Fatalf("Submit(TypeShareRelocation): %v", err)
	}
	finished, err := scheduler.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusFailed {
		t.Fatalf("status = %s, want failed (real threshold guard trip)", finished.Status)
	}
	if !strings.Contains(finished.ErrorMessage, "threshold guard blocked the sync") {
		t.Fatalf("ErrorMessage = %q, want a real threshold-guard block", finished.ErrorMessage)
	}

	if _, err := os.Stat(src1); err != nil {
		t.Fatalf("array original 1 must survive a blocked sync: %v", err)
	}
	if _, err := os.Stat(src2); err != nil {
		t.Fatalf("array original 2 must survive a blocked sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, shareName, "one.bin")); err != nil {
		t.Fatalf("verified cache copy 1 must survive a blocked sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, shareName, "two.bin")); err != nil {
		t.Fatalf("verified cache copy 2 must survive a blocked sync: %v", err)
	}

	diff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after blocked relocation: %v", err)
	}
	if diff.Removed < 2 {
		t.Fatalf("Diff after blocked relocation: Removed = %d, want >=2 — parity must still see the array originals as pending removal, unsynced", diff.Removed)
	}
}
