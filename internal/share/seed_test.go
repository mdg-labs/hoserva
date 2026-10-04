package share

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// writeRecordingFS fails the test on any call that could write to a branch
// directory, and counts the calls so a test can say there were none.
type writeRecordingFS struct {
	OSFS
	calls []string
}

func (f *writeRecordingFS) MkdirAll(path string, perm os.FileMode) error {
	f.calls = append(f.calls, "MkdirAll "+path)
	return f.OSFS.MkdirAll(path, perm)
}

func (f *writeRecordingFS) Chmod(path string, mode os.FileMode) error {
	f.calls = append(f.calls, "Chmod "+path)
	return f.OSFS.Chmod(path, mode)
}

func (f *writeRecordingFS) Chown(path string, uid, gid int) error {
	f.calls = append(f.calls, "Chown "+path)
	return f.OSFS.Chown(path, uid, gid)
}

func (f *writeRecordingFS) RemoveAll(path string) error {
	f.calls = append(f.calls, "RemoveAll "+path)
	return f.OSFS.RemoveAll(path)
}

func (f *writeRecordingFS) RemoveConfined(root, rel string) error {
	f.calls = append(f.calls, "RemoveConfined "+root+" "+rel)
	return f.OSFS.RemoveConfined(root, rel)
}

// pendingService is a share service over the array of a pending Unraid
// migration: two adopted data disks, no cache, and a share directory already
// on disk1 only, as an Unraid share is.
func pendingService(t *testing.T) (context.Context, *Service, testLayout, *recordingMounter, *writeRecordingFS) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	layout := testLayout{
		dataDisks: []string{filepath.Join(root, "disk1"), filepath.Join(root, "disk2")},
		catchAll:  filepath.Join(root, "user"),
	}
	for _, d := range append(append([]string{}, layout.dataDisks...), layout.catchAll) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(layout.dataDisks[0], "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	shareStore, arrayStore := testStores(t)
	if err := arrayStore.PutPendingArray(ctx, store.ArraySettings{
		CreatePolicy: string(pool.DefaultCreatePolicy),
		MinFreeSpace: "50G",
		CreatedAt:    time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: layout.dataDisks[0]},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", Mountpoint: layout.dataDisks[1]},
	}, nil); err != nil {
		t.Fatalf("PutPendingArray: %v", err)
	}
	mounter := &recordingMounter{}
	fs := &writeRecordingFS{}
	svc := &Service{
		Shares:   shareStore,
		Array:    arrayStore,
		Gen:      config.NewGenerator(filepath.Join(root, "etc")),
		FS:       fs,
		Mounter:  mounter,
		Now:      func() time.Time { return time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC) },
		CatchAll: layout.catchAll,
	}
	return ctx, svc, layout, mounter, fs
}

func seedInput() SeedInput {
	return SeedInput{
		Users: []SeedUser{{Username: "alice", PasswordHash: "x"}, {Username: "bob", PasswordHash: "x"}},
		Shares: []SeedShare{
			{
				Name: "media", CreatePolicy: pool.FillDisksInOrder, TargetCacheMode: pool.CacheThenMove,
				SMB: SMB{Enabled: true, Browseable: true}, MinFreeSpace: "1000K", Notes: []string{"a note"},
				Access: []SeedAccess{{"alice", "read-write"}, {"bob", "read-only"}},
			},
			{Name: "docs", CreatePolicy: pool.BalanceAcrossDisks, SMB: SMB{Enabled: true, Browseable: true, Guest: true}},
		},
	}
}

// The data-loss scenario: seeding shares must never create, chmod or chown a
// directory on an adopted branch, with a cache mode that needs a cache disk
// that does not exist, and with a share directory present on one disk only.
func TestSeedMigration_WritesNothingToAnAdoptedBranch(t *testing.T) {
	ctx, svc, layout, mounter, fs := pendingService(t)
	res, err := svc.SeedMigration(ctx, seedInput())
	if err != nil {
		t.Fatalf("SeedMigration: %v", err)
	}
	if len(fs.calls) != 0 {
		t.Errorf("seeding made filesystem write calls on the adopted disks: %v", fs.calls)
	}
	for _, p := range []string{filepath.Join(layout.dataDisks[1], "media"), filepath.Join(layout.dataDisks[0], "docs"), filepath.Join(layout.dataDisks[1], "docs")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("seeding created %s on an adopted disk", p)
		}
	}
	if !reflect.DeepEqual(res.Shares, []string{"media", "docs"}) || !reflect.DeepEqual(res.Users, []string{"alice", "bob"}) {
		t.Errorf("result = %+v", res)
	}

	got, err := svc.Get(ctx, "media")
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheMode != pool.ArrayOnly || got.TargetCacheMode != pool.CacheThenMove || got.MinFreeSpace != "1000K" ||
		got.CreatePolicy != pool.FillDisksInOrder || !reflect.DeepEqual(got.MigrationNotes, []string{"a note"}) {
		t.Errorf("media = %+v", got)
	}

	// A share is a directory of the read-only catch-all: no mount of its own
	// and no unit for it or a mover target, which a share without a directory
	// on any adopted disk could not have.
	if len(mounter.mounts) != 0 {
		t.Errorf("seeding mounted %v", mounter.mounts)
	}
	units, _ := filepath.Glob(filepath.Join(svc.Gen.Root, "systemd", "system", "*.mount"))
	if len(units) != 1 || filepath.Base(units[0]) != "mnt-user.mount" {
		t.Fatalf("units = %v, want only the read-only catch-all", units)
	}
	unit := readFile(t, units[0])
	if !strings.Contains(unit, "=RO") || strings.Contains(unit, "=RW") || strings.Contains(unit, "=NC") || !strings.Contains(unit+"\n", ",ro\n") {
		t.Errorf("mnt-user.mount is not read-only:\n%s", unit)
	}
	smb := readFile(t, filepath.Join(svc.Gen.Root, "samba", "smb.conf"))
	if !strings.Contains(smb, "[media]") || !strings.Contains(smb, "[docs]") || strings.Contains(smb, "read only = no") {
		t.Errorf("smb.conf must export both shares read-only:\n%s", smb)
	}
}

// After the point of no return the shares would be read-write again, so the
// read-only export is generated from the pending state and never stored in the
// share.
func TestSeedMigration_ReadOnlyExportIsNotStoredInTheShare(t *testing.T) {
	ctx, svc, _, _, _ := pendingService(t)
	if _, err := svc.SeedMigration(ctx, seedInput()); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if got.SMB.ReadOnly || !got.SMB.Guest || !got.SMB.Enabled {
		t.Errorf("docs SMB = %+v, want the import's own settings and not read-only", got.SMB)
	}
}

func TestSeedMigration_RefusesAnArrayThatIsNotPending(t *testing.T) {
	ctx, svc, _, mounter, fs := testServiceWithFS(t)
	_, err := svc.SeedMigration(ctx, seedInput())
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("SeedMigration on a settled array = %v, want ErrInvalidInput", err)
	}
	if len(fs.calls) != 0 || len(mounter.mounts) != 0 {
		t.Errorf("a refused seed wrote: fs %v mounts %v", fs.calls, mounter.mounts)
	}
	if rows, _ := svc.Shares.List(ctx); len(rows) != 0 {
		t.Errorf("a refused seed left %d shares", len(rows))
	}
}

func testServiceWithFS(t *testing.T) (context.Context, *Service, testLayout, *recordingMounter, *writeRecordingFS) {
	t.Helper()
	ctx, svc, layout, mounter := testService(t)
	fs := &writeRecordingFS{}
	svc.FS = fs
	return ctx, svc, layout, mounter, fs
}

// Everything is checked before the first write: a bad share anywhere in the
// list seeds none of them and creates no user.
func TestSeedMigration_ValidatesEverythingBeforeTheFirstWrite(t *testing.T) {
	cases := map[string]func(in *SeedInput){
		"a share name with a space":    func(in *SeedInput) { in.Shares[1].Name = "my share" },
		"a duplicate share":            func(in *SeedInput) { in.Shares[1].Name = "media" },
		"an unknown create policy":     func(in *SeedInput) { in.Shares[1].CreatePolicy = "bogus" },
		"an unknown target mode":       func(in *SeedInput) { in.Shares[1].TargetCacheMode = "bogus" },
		"a minimum free space":         func(in *SeedInput) { in.Shares[1].MinFreeSpace = "1000 K" },
		"a grant to an unknown user":   func(in *SeedInput) { in.Shares[1].Access = []SeedAccess{{"carol", "read-only"}} },
		"an unknown access level":      func(in *SeedInput) { in.Shares[1].Access = []SeedAccess{{"alice", "admin"}} },
		"a user without a placeholder": func(in *SeedInput) { in.Users[0].PasswordHash = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, svc, _, mounter, fs := pendingService(t)
			in := seedInput()
			mutate(&in)
			if _, err := svc.SeedMigration(ctx, in); !errors.Is(err, ErrInvalidInput) && !errors.Is(err, ErrInvalidName) {
				t.Fatalf("SeedMigration = %v, want a refusal of the input", err)
			}
			if rows, _ := svc.Shares.List(ctx); len(rows) != 0 {
				t.Errorf("%d shares were seeded before the refusal", len(rows))
			}
			if len(fs.calls) != 0 || len(mounter.mounts) != 0 {
				t.Errorf("a refused seed wrote: fs %v mounts %v", fs.calls, mounter.mounts)
			}
			if _, err := os.Stat(filepath.Join(svc.Gen.Root, "samba", "smb.conf")); err == nil {
				t.Error("smb.conf was written by a refused seed")
			}
		})
	}
}

// A host smb.conf Hoserva has not taken over refuses the seed, as it refuses a
// share create, before any file is written, and the rows are gone again: no
// share, no account, no unit.
func TestSeedMigration_AFileThatCannotBeWrittenLeavesNothingBehind(t *testing.T) {
	ctx, svc, _, mounter, fs := pendingService(t)
	writeFile(t, filepath.Join(svc.Gen.Root, "samba", "smb.conf"), "[global]\n   workgroup = HOME\n")
	_, err := svc.SeedMigration(ctx, seedInput())
	if !errors.Is(err, config.ErrExistingHostFile) {
		t.Fatalf("SeedMigration = %v, want ErrExistingHostFile", err)
	}
	if rows, _ := svc.Shares.List(ctx); len(rows) != 0 {
		t.Errorf("%d shares are left after the failed seed", len(rows))
	}
	if units, _ := filepath.Glob(filepath.Join(svc.Gen.Root, "systemd", "system", "*.mount")); len(units) != 0 {
		t.Errorf("units written by a refused seed: %v", units)
	}
	if got := readFile(t, filepath.Join(svc.Gen.Root, "samba", "smb.conf")); !strings.Contains(got, "HOME") {
		t.Errorf("the host's smb.conf was replaced:\n%s", got)
	}
	if len(fs.calls) != 0 || len(mounter.mounts) != 0 {
		t.Errorf("a failed seed wrote: fs %v mounts %v", fs.calls, mounter.mounts)
	}

	// Nothing of the first attempt is left: the retry creates both accounts
	// and both shares as new ones.
	if err := os.Remove(filepath.Join(svc.Gen.Root, "samba", "smb.conf")); err != nil {
		t.Fatal(err)
	}
	res, err := svc.SeedMigration(ctx, seedInput())
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if len(res.Users) != 2 || len(res.ExistingUsers) != 0 || len(res.Shares) != 2 || len(res.ExistingShares) != 0 {
		t.Errorf("the retry = %+v, want everything created anew", res)
	}
}

// A seed that fails after its files were written is undone: the rows go, and
// smb.conf is regenerated without the shares.
func TestSeedMigration_UndoingAfterTheFilesWereWrittenRegeneratesThem(t *testing.T) {
	ctx, svc, _, _, _ := pendingService(t)
	users, shares, err := svc.seedRows(seedInput())
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := svc.Shares.SeedMigration(ctx, users, shares)
	if err != nil {
		t.Fatal(err)
	}
	if written, err := svc.applySeeded(ctx); err != nil || !written {
		t.Fatalf("applySeeded = %v, %v", written, err)
	}
	if smb := readFile(t, filepath.Join(svc.Gen.Root, "samba", "smb.conf")); !strings.Contains(smb, "[media]") {
		t.Fatalf("smb.conf lacks the share before the undo:\n%s", smb)
	}
	cause := errors.New("injected: a later step failed")
	if err := svc.undoSeed(ctx, seeded, true, cause); !errors.Is(err, cause) || strings.Contains(err.Error(), "could not be restored") {
		t.Fatalf("undoSeed = %v, want the cause alone", err)
	}
	if rows, _ := svc.Shares.List(ctx); len(rows) != 0 {
		t.Errorf("%d shares are left after the undo", len(rows))
	}
	if smb := readFile(t, filepath.Join(svc.Gen.Root, "samba", "smb.conf")); strings.Contains(smb, "[media]") || strings.Contains(smb, "[docs]") {
		t.Errorf("smb.conf still exports a seeded share after the undo:\n%s", smb)
	}
}

// A share or user that exists is left as it is, and is not unseeded by a retry
// that fails.
func TestSeedMigration_LeavesExistingSharesAlone(t *testing.T) {
	ctx, svc, _, _, _ := pendingService(t)
	if _, err := svc.SeedMigration(ctx, seedInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, "docs", UpdateInput{SMB: &SMB{Enabled: true, Browseable: false}}); err != nil {
		t.Fatalf("Update while pending: %v", err)
	}
	res, err := svc.SeedMigration(ctx, seedInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Shares) != 0 || !reflect.DeepEqual(res.ExistingShares, []string{"media", "docs"}) {
		t.Errorf("second seed = %+v, want both shares reported as existing", res)
	}
	docs, _ := svc.Get(ctx, "docs")
	if docs.SMB.Browseable {
		t.Error("the second seed overwrote an existing share")
	}
}

// The pending array's other share operations are the same no-write ones.
func TestPendingMigration_ShareOperationsNeverWriteToAdoptedBranches(t *testing.T) {
	ctx, svc, layout, mounter, fs := pendingService(t)
	if _, err := svc.SeedMigration(ctx, seedInput()); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Create(ctx, CreateInput{Name: "fresh", CacheMode: pool.ArrayOnly}); !errors.Is(err, ErrMigrationPending) {
		t.Errorf("Create while pending = %v, want ErrMigrationPending", err)
	}
	if err := svc.DeleteData(ctx, "media", "media"); !errors.Is(err, ErrMigrationPending) {
		t.Errorf("DeleteData while pending = %v, want ErrMigrationPending", err)
	}
	if _, err := os.Stat(filepath.Join(layout.dataDisks[0], "media")); err != nil {
		t.Errorf("the share's directory is gone: %v", err)
	}

	mode := pool.ArrayOnly
	if _, err := svc.Update(ctx, "media", UpdateInput{CacheMode: &mode}); err != nil {
		t.Fatalf("Update while pending: %v", err)
	}
	updated, _ := svc.Get(ctx, "media")
	if updated.TargetCacheMode != "" {
		t.Errorf("an explicit cache mode kept the import's target %q", updated.TargetCacheMode)
	}
	cache := pool.CacheOnly
	if _, err := svc.Update(ctx, "media", UpdateInput{CacheMode: &cache}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Update to a cache mode while pending = %v, want ErrInvalidInput (no cache disk)", err)
	}
	if err := svc.ApplyTopology(ctx, true); err != nil {
		t.Fatalf("ApplyTopology(live) while pending: %v", err)
	}
	if err := svc.Delete(ctx, "docs", true); err != nil {
		t.Fatalf("Delete while pending: %v", err)
	}

	if len(fs.calls) != 0 {
		t.Errorf("share operations wrote to the adopted disks: %v", fs.calls)
	}
	for _, p := range []string{filepath.Join(layout.dataDisks[1], "media"), filepath.Join(layout.dataDisks[0], "docs"), filepath.Join(layout.dataDisks[1], "docs")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was created on an adopted disk", p)
		}
	}
	if len(mounter.mounts) != 0 {
		t.Errorf("a share operation mounted %v while pending", mounter.mounts)
	}
	units, _ := filepath.Glob(filepath.Join(svc.Gen.Root, "systemd", "system", "*.mount"))
	if len(units) != 1 || filepath.Base(units[0]) != "mnt-user.mount" {
		t.Errorf("units = %v, want only the read-only catch-all", units)
	}
	for _, u := range units {
		if body := readFile(t, u); strings.Contains(body, "=RW") || strings.Contains(body, "=NC") || !strings.Contains(body+"\n", ",ro\n") {
			t.Errorf("%s is writable:\n%s", u, body)
		}
	}
}
