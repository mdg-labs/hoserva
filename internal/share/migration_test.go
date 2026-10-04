package share

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// migratedShare inserts a share the way the import seeds one: array-only, with
// the Unraid cache mode recorded as its target.
func migratedShare(t *testing.T, svc *Service, name string, target pool.CacheMode) {
	t.Helper()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	rec := toStore(Share{
		Name: name, CacheMode: pool.ArrayOnly, CreatePolicy: pool.DefaultCreatePolicy, TargetCacheMode: target,
		SMB: defaultSMB(), NFS: defaultNFS(), CreatedAt: now, UpdatedAt: now,
	})
	if err := svc.Shares.Insert(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
}

// The share step of the point of no return applies the cache mode the import
// deferred and brings each top-level share directory, and only that, to the
// shared group and setgid mode (Q26); a file under it is not touched.
func TestCompleteMigration_AppliesTheDeferredCacheModeAndTheTopLevelDirectoryModeOnly(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	fs := svc.FS.(*ownerRecordingFS)
	migratedShare(t, svc, "media", pool.CacheThenMove)
	migratedShare(t, svc, "docs", "")
	// An Unraid share directory is world-writable and owned by nobody:users; a
	// file in it must keep exactly what it has.
	mediaOnDisk1 := filepath.Join(layout.dataDisks[0], "media")
	if err := os.MkdirAll(mediaOnDisk1, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mediaOnDisk1, 0o777); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(mediaOnDisk1, "film.mkv")
	if err := os.WriteFile(file, []byte("frames"), 0o640); err != nil {
		t.Fatal(err)
	}

	res, err := svc.CompleteMigration(ctx)
	if err != nil {
		t.Fatalf("CompleteMigration: %v", err)
	}
	if !reflect.DeepEqual(res.Applied, []string{"media: array-only -> cache-then-move"}) || len(res.Kept) != 0 {
		t.Errorf("result = %+v", res)
	}

	got, err := svc.Get(ctx, "media")
	if err != nil || got.CacheMode != pool.CacheThenMove || got.TargetCacheMode != "" {
		t.Errorf("media = %+v, %v, want the target applied and cleared", got, err)
	}
	if docs, err := svc.Get(ctx, "docs"); err != nil || docs.CacheMode != pool.ArrayOnly || docs.TargetCacheMode != "" {
		t.Errorf("docs = %+v, %v, want it left array-only", docs, err)
	}

	for _, dir := range []string{
		filepath.Join(layout.cache, "media"), filepath.Join(layout.dataDisks[0], "media"), filepath.Join(layout.dataDisks[1], "media"),
		filepath.Join(layout.dataDisks[0], "docs"), filepath.Join(layout.dataDisks[1], "docs"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("%s was not made: %v", dir, err)
			continue
		}
		if info.Mode().Perm() != 0o775 || info.Mode()&os.ModeSetgid == 0 {
			t.Errorf("%s has mode %v, want rwxrwsr-x (Q26)", dir, info.Mode())
		}
	}
	if _, err := os.Stat(filepath.Join(layout.cache, "docs")); err == nil {
		t.Error("an array-only share got a directory on the cache")
	}
	for _, c := range fs.chowns {
		if c.uid != -1 || c.gid != ShareGID {
			t.Errorf("chown %+v, want the group only (gid %d)", c, ShareGID)
		}
		if c.path == file {
			t.Errorf("the file %s was chowned: only top-level directories are", file)
		}
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("the file under the share has mode %v, %v, want 0640 untouched", info.Mode(), err)
	}
	if b, _ := os.ReadFile(file); string(b) != "frames" {
		t.Errorf("the file under the share changed: %q", b)
	}

	again, err := svc.CompleteMigration(ctx)
	if err != nil || len(again.Applied) != 0 {
		t.Errorf("a second run = %+v, %v, want nothing left to apply", again, err)
	}
}

func TestCompleteMigration_AModeThatNeedsACacheStaysRecordedWhenThereIsNone(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	disk1, disk2 := filepath.Join(root, "disk1"), filepath.Join(root, "disk2")
	for _, d := range []string{disk1, disk2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shares, arrays := testStores(t)
	if err := arrays.PutArray(ctx, store.ArraySettings{CreatePolicy: string(pool.DefaultCreatePolicy), MinFreeSpace: "50G", CreatedAt: time.Now()}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "u1", Mountpoint: disk1},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "u2", Mountpoint: disk2},
	}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Shares: shares, Array: arrays, FS: &ownerRecordingFS{}, Now: time.Now}
	migratedShare(t, svc, "appdata", pool.CacheOnly)

	res, err := svc.CompleteMigration(ctx)
	if err != nil {
		t.Fatalf("CompleteMigration: %v", err)
	}
	if len(res.Applied) != 0 || len(res.Kept) != 1 {
		t.Fatalf("result = %+v, want the cache-only share kept", res)
	}
	got, err := svc.Get(ctx, "appdata")
	if err != nil || got.CacheMode != pool.ArrayOnly || got.TargetCacheMode != pool.CacheOnly {
		t.Errorf("appdata = %+v, %v, want it array-only with cache-only still recorded", got, err)
	}
	for _, d := range []string{disk1, disk2} {
		if info, err := os.Stat(filepath.Join(d, "appdata")); err != nil || info.Mode()&os.ModeSetgid == 0 {
			t.Errorf("%s/appdata = %v, %v, want its array directory prepared", d, info, err)
		}
	}
}

func TestCompleteMigration_IsRefusedWhileTheMigrationIsPending(t *testing.T) {
	ctx, svc, layout, _, fs := pendingService(t)
	migratedShare(t, svc, "media", pool.CacheThenMove)
	if _, err := svc.CompleteMigration(ctx); !errors.Is(err, ErrMigrationPending) {
		t.Fatalf("CompleteMigration while pending = %v, want ErrMigrationPending", err)
	}
	if len(fs.calls) != 0 {
		t.Errorf("a refused CompleteMigration wrote to the adopted disks: %v", fs.calls)
	}
	if _, err := os.Stat(filepath.Join(layout.dataDisks[1], "media")); err == nil {
		t.Error("a directory appeared on an adopted disk while the migration is pending")
	}
}
