package share

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
	_ "modernc.org/sqlite"
)

type testLayout struct {
	dataDisks []string
	cache     string
	parity    string
	catchAll  string
}

type recordingMounter struct {
	mounts   []string
	unmounts []string
}

func (m *recordingMounter) Mount(_ context.Context, mnt pool.Mount) error {
	m.mounts = append(m.mounts, mnt.Where)
	return nil
}

func (m *recordingMounter) Unmount(_ context.Context, where string) error {
	m.unmounts = append(m.unmounts, where)
	return nil
}

type xattrFS struct {
	OSFS
	xattr map[string]string
}

func (f xattrFS) GetXattr(path, attr string) ([]byte, error) {
	if attr != MergerFSBasepath {
		return nil, nil
	}
	if v, ok := f.xattr[path]; ok {
		return []byte(v), nil
	}
	return nil, nil
}

func testService(t *testing.T) (context.Context, *Service, testLayout, *recordingMounter) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	layout := testLayout{
		dataDisks: []string{filepath.Join(root, "disk1"), filepath.Join(root, "disk2")},
		cache:     filepath.Join(root, "cache"),
		parity:    filepath.Join(root, "parity1"),
		catchAll:  filepath.Join(root, "user"),
	}
	for _, d := range append(append([]string{}, layout.dataDisks...), layout.cache, layout.parity, layout.catchAll) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	shareStore, arrayStore := testStores(t)
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: string(pool.DefaultCreatePolicy),
		MinFreeSpace: "50G",
		CreatedAt:    time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: layout.parity},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: layout.dataDisks[0]},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", Mountpoint: layout.dataDisks[1]},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdd", Filesystem: "ext4", FSUUID: "uuid-c", Mountpoint: layout.cache},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	mounter := &recordingMounter{}
	svc := &Service{
		Shares:   shareStore,
		Array:    arrayStore,
		Gen:      config.NewGenerator(filepath.Join(root, "etc")),
		FS:       OSFS{},
		Mounter:  mounter,
		Now:      func() time.Time { return time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC) },
		CatchAll: layout.catchAll,
	}
	return ctx, svc, layout, mounter
}

func testStores(t *testing.T) (*store.ShareStore, *store.ArrayStore) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "shares.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store.NewShareStore(db), store.NewArrayStore(db)
}

func createTestShare(t *testing.T, svc *Service, name string, mode pool.CacheMode) {
	t.Helper()
	if _, err := svc.Create(context.Background(), CreateInput{Name: name, CacheMode: mode}); err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
