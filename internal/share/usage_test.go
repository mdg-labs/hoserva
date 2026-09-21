package share

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// usageTestService builds a Service backed by real stores (shares and
// share usage, both through the same embedded-migration SQLite database)
// — Get/List's Usage attachment (#223) reads through parity.UsageStore
// exactly as the daemon wires it, never a fake.
func usageTestService(t *testing.T) (context.Context, *Service, *store.ShareStore, *parity.UsageStore) {
	t.Helper()
	ctx := context.Background()
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "shares-usage.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	shareStore := store.NewShareStore(db)
	usages := parity.NewUsageStore(db)
	svc := &Service{Shares: shareStore, Usages: usages}
	return ctx, svc, shareStore, usages
}

func insertTestShareRow(t *testing.T, ctx context.Context, shares *store.ShareStore, name string, createdAt time.Time) {
	t.Helper()
	err := shares.Insert(ctx, store.Share{
		Name:         name,
		CacheMode:    string(pool.ArrayOnly),
		CreatePolicy: string(pool.DefaultCreatePolicy),
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	})
	if err != nil {
		t.Fatalf("inserting share %s: %v", name, err)
	}
}

func TestService_Get_NeverSyncedReportsHonestState(t *testing.T) {
	ctx, svc, shares, _ := usageTestService(t)
	insertTestShareRow(t, ctx, shares, "movies", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))

	sh, err := svc.Get(ctx, "movies")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sh.Usage.Synced {
		t.Fatalf("Usage.Synced = true before any computation ran: %+v", sh.Usage)
	}
}

func TestService_Get_CreatedAfterLastComputationIsNotYetSynced(t *testing.T) {
	ctx, svc, shares, usages := usageTestService(t)
	computedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := usages.Replace(ctx, []parity.ShareUsage{{Share: "movies", Disk: "/mnt/disk1", Bytes: 100}}, computedAt); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	// A share created after the last computation has never had a chance
	// to be included in it, even though the store has computed something.
	insertTestShareRow(t, ctx, shares, "movies", computedAt.Add(time.Hour))

	sh, err := svc.Get(ctx, "movies")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sh.Usage.Synced {
		t.Fatalf("Usage.Synced = true for a share created after the last computation: %+v", sh.Usage)
	}
}

func TestService_Get_SyncedZeroBytesIsHonestZero(t *testing.T) {
	ctx, svc, shares, usages := usageTestService(t)
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	insertTestShareRow(t, ctx, shares, "empty-share", created)
	computedAt := created.Add(time.Hour)
	// Replace with a different share's rows only — empty-share genuinely
	// has no files on any disk after this (real) computation.
	if err := usages.Replace(ctx, []parity.ShareUsage{{Share: "other", Disk: "/mnt/disk1", Bytes: 100}}, computedAt); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	sh, err := svc.Get(ctx, "empty-share")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !sh.Usage.Synced {
		t.Fatalf("Usage.Synced = false for a share genuinely computed with zero bytes: %+v", sh.Usage)
	}
	if sh.Usage.TotalBytes != 0 || len(sh.Usage.Disks) != 0 {
		t.Fatalf("Usage = %+v, want zero bytes on no disks", sh.Usage)
	}
	if !sh.Usage.AsOf.Equal(computedAt) {
		t.Fatalf("Usage.AsOf = %v, want %v", sh.Usage.AsOf, computedAt)
	}
}

func TestService_Get_SyncedWithBytesReportsPerDiskBreakdown(t *testing.T) {
	ctx, svc, shares, usages := usageTestService(t)
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	insertTestShareRow(t, ctx, shares, "movies", created)
	computedAt := created.Add(time.Hour)
	if err := usages.Replace(ctx, []parity.ShareUsage{
		{Share: "movies", Disk: "/mnt/disk1", Bytes: 100},
		{Share: "movies", Disk: "/mnt/disk2", Bytes: 200},
	}, computedAt); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	sh, err := svc.Get(ctx, "movies")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !sh.Usage.Synced {
		t.Fatal("Usage.Synced = false")
	}
	if sh.Usage.TotalBytes != 300 {
		t.Fatalf("Usage.TotalBytes = %d, want 300", sh.Usage.TotalBytes)
	}
	if sh.Usage.Disks["/mnt/disk1"] != 100 || sh.Usage.Disks["/mnt/disk2"] != 200 {
		t.Fatalf("Usage.Disks = %+v", sh.Usage.Disks)
	}
}

func TestService_List_AttachesUsagePerShareInOneBatch(t *testing.T) {
	ctx, svc, shares, usages := usageTestService(t)
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	insertTestShareRow(t, ctx, shares, "movies", created)
	insertTestShareRow(t, ctx, shares, "new-share", created.Add(48*time.Hour))
	computedAt := created.Add(time.Hour)
	if err := usages.Replace(ctx, []parity.ShareUsage{
		{Share: "movies", Disk: "/mnt/disk1", Bytes: 100},
	}, computedAt); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byName := map[string]Share{}
	for _, sh := range list {
		byName[sh.Name] = sh
	}
	if !byName["movies"].Usage.Synced || byName["movies"].Usage.TotalBytes != 100 {
		t.Fatalf("movies Usage = %+v", byName["movies"].Usage)
	}
	if byName["new-share"].Usage.Synced {
		t.Fatalf("new-share Usage.Synced = true for a share created after the last computation: %+v", byName["new-share"].Usage)
	}
}

func TestService_Get_NilUsagesIsNeverSynced(t *testing.T) {
	ctx := context.Background()
	shares, _ := testStores(t)
	svc := &Service{Shares: shares}
	insertTestShareRow(t, ctx, shares, "movies", time.Now())

	sh, err := svc.Get(ctx, "movies")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sh.Usage.Synced {
		t.Fatal("Usage.Synced = true with no Usages store wired at all")
	}
}
