package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// storageTargetTestMount is job.ArrayMount's own fake for this file's own
// tests — Where() builds the target's own Wants=/After= disk mount list
// and identifies the catch-all for confirmPoolMounted; Mount() records
// how many times it ran and returns mountErr, so a test can both assert
// mountPool actually called it and simulate a mount failure.
type storageTargetTestMount struct {
	where    string
	mountErr error
	calls    *int32
}

func (m storageTargetTestMount) Where() string { return m.where }
func (m storageTargetTestMount) Mount(context.Context) error {
	if m.calls != nil {
		atomic.AddInt32(m.calls, 1)
	}
	return m.mountErr
}
func (m storageTargetTestMount) Unmount(context.Context) error { return nil }

// newTestScheduler is a real *job.Scheduler backed by a throwaway SQLite
// database — EnterMaintenance/InMaintenance are Scheduler's own methods,
// not behind an interface this package can fake, so the not-ready→ready-
// during-maintenance test below needs a real one, the same way
// cmd/hoservad's other tests already build one (array_test.go).
func newTestScheduler(t *testing.T) *job.Scheduler {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "storagetarget-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return job.NewScheduler(job.NewStore(db), job.NewLogStore(t.TempDir()), job.NewHub(), job.NewRegistry())
}

// newTestArrayAndShareStore is a real *store.ArrayStore/*store.ShareStore
// pair backed by a throwaway SQLite database, for
// TestStorageTargetSync_Startup_RegeneratesArrayMountsFromStore: unlike
// newTestScheduler above, job.RegenerateArrayMountsFromStore reads these
// two directly, not through a fake.
func newTestArrayAndShareStore(t *testing.T) (*store.ArrayStore, *store.ShareStore) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "storagetarget-store-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return store.NewArrayStore(db), store.NewShareStore(db)
}

// seedTestArray puts a minimal one-parity, two-data-disk array into st
// (parity.Layout.ContentPaths' own Q18 minimum: parity-disks+2 distinct
// devices for the content file), for
// TestStorageTargetSync_Startup_RegeneratesArrayMountsFromStore.
func seedTestArray(t *testing.T, ctx context.Context, st *store.ArrayStore) {
	t.Helper()
	settings := store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Now().UTC()}
	disks := []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", Mountpoint: "/mnt/disk2"},
	}
	if err := st.PutArray(ctx, settings, disks); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
}

// storageTargetTestGate is job.ReadinessGate's own fake.
type storageTargetTestGate struct{ ready bool }

func (g storageTargetTestGate) Ready() bool { return g.ready }

func newTestStorageTargetSync(t *testing.T) *storageTargetSync {
	t.Helper()
	return &storageTargetSync{
		Generator:       cfggen.NewGenerator(t.TempDir()),
		Runner:          disk.NewFakeRunner(),
		FlagPath:        filepath.Join(t.TempDir(), "storage-ready"),
		StoppedFlagPath: filepath.Join(t.TempDir(), "array-stopped"),
	}
}

func hasCall(calls []disk.RunCall, name string, args ...string) bool {
	for _, c := range calls {
		if c.Name != name || len(c.Args) != len(args) {
			continue
		}
		match := true
		for i := range args {
			if c.Args[i] != args[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func hasCallWithVerb(calls []disk.RunCall, verbs ...string) bool {
	for _, c := range calls {
		if c.Name != "systemctl" || len(c.Args) == 0 {
			continue
		}
		for _, verb := range verbs {
			if c.Args[0] == verb {
				return true
			}
		}
	}
	return false
}

func boolLabel(b bool) string {
	if b {
		return "ready"
	}
	return "not-ready"
}

func TestStorageTargetSync_NilSequenceIsNoOp(t *testing.T) {
	s := newTestStorageTargetSync(t)

	if err := s.Startup(context.Background(), nil); err != nil {
		t.Fatalf("Startup(nil) = %v, want nil", err)
	}
	s.Update(context.Background(), nil)

	fakeRunner := s.Runner.(*disk.FakeRunner)
	if calls := fakeRunner.Calls(); len(calls) != 0 {
		t.Fatalf("Calls() = %v, want none for a nil sequence", calls)
	}
	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag was written for a nil sequence")
	}
}

// TestStorageTargetSync_Startup_MountsPoolButNeverStartsAUnitAfterHoservad
// is the corrected regression test for both halves of Startup's own
// contract. Half 1 (the actual #372 regression, nightly L3 run
// 36226407069): an ordinary boot must bring the catch-all and every
// share mount up itself — the generated pool mount units carry no
// [Install] section, so nothing else ever calls Mount() on them, and a
// version of this fix that made Startup only ever *confirm* left every
// share unmounted forever after a real reboot, with smbd/nfs-kernel-
// server reporting active over an unmounted /mnt/user/massdel. Half 2
// (#387, still true): whatever the gate says, Startup never issues
// a systemctl start or restart of a unit ordered After=hoserva.service —
// only that ordering keeps hoserva-storage-ready.service from
// deadlocking this process before it can send systemd's own READY=1
// (reproduced for real in the nightly L3 workflow, run 36197197961,
// against an earlier version of this fix that called systemctl restart
// from this call site). Mounting the pool's own units carries no such
// risk, since they are not ordered after hoserva.service at all.
func TestStorageTargetSync_Startup_MountsPoolButNeverStartsAUnitAfterHoservad(t *testing.T) {
	for _, ready := range []bool{true, false} {
		t.Run(boolLabel(ready), func(t *testing.T) {
			s := newTestStorageTargetSync(t)
			s.PoolMounted = func(string) (bool, error) { return true, nil }
			var catchAllCalls, shareCalls int32
			catchAll := storageTargetTestMount{where: "/mnt/user", calls: &catchAllCalls}
			shareMount := storageTargetTestMount{where: "/mnt/user/massdel", calls: &shareCalls}
			seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: ready}, CatchAll: catchAll, ShareMounts: []job.ArrayMount{shareMount}}

			if err := s.Startup(context.Background(), seq); err != nil {
				t.Fatalf("Startup: %v", err)
			}

			if ready {
				if atomic.LoadInt32(&catchAllCalls) != 1 {
					t.Fatalf("CatchAll.Mount() called %d times, want 1 — an ordinary boot must bring the pool up itself; nothing else does (the generated mount units carry no [Install])", catchAllCalls)
				}
				if atomic.LoadInt32(&shareCalls) != 1 {
					t.Fatalf("share Mount() called %d times, want 1", shareCalls)
				}
			}
			fakeRunner := s.Runner.(*disk.FakeRunner)
			if hasCallWithVerb(fakeRunner.Calls(), "start", "restart") {
				t.Fatalf("Calls() = %v, want no systemctl start/restart of a unit ordered after hoserva.service from Startup", fakeRunner.Calls())
			}
			if !hasCall(fakeRunner.Calls(), "systemctl", "daemon-reload") {
				t.Fatalf("Calls() = %v, want a systemctl daemon-reload", fakeRunner.Calls())
			}

			_, err := os.Stat(s.flagPath())
			if ready && err != nil {
				t.Fatalf("Stat(flag) = %v, want the flag to exist once ready", err)
			}
			if !ready && err == nil {
				t.Fatal("the readiness flag exists while the gate refuses readiness")
			}
		})
	}
}

// TestStorageTargetSync_Startup_MountsDisksEvenIfSkippedByABootTimeCondition
// proves (#387) a physical disk's nofail .mount
// unit is ordinarily expected to activate on its own the moment its
// device appears, well before Startup ever runs — but a boot where the
// array-stopped condition flag was still set from before this boot's own
// reconciliation (already run ahead of this point) skips that automatic
// activation silently, in the very same boot-time systemd transaction,
// and nothing else ever retries it. Startup must bring every physical
// disk mount up itself rather than assume the automatic mount already
// happened just because the gate now reports every expected disk present.
func TestStorageTargetSync_Startup_MountsDisksEvenIfSkippedByABootTimeCondition(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	var diskCalls int32
	disk1 := storageTargetTestMount{where: "/mnt/disk1", calls: &diskCalls}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	if atomic.LoadInt32(&diskCalls) != 1 {
		t.Fatalf("disk Mount() called %d times, want 1 — Startup must bring every physical disk mount up itself (#387)", diskCalls)
	}
}

// TestStorageTargetSync_Startup_InMaintenanceDoesNotMountDisks proves the
// other half of the above: while the array is in maintenance mode at
// startup, Startup must not mount the disks either — a hoservad that
// restarts mid-swap must not remount storage the user explicitly took
// down, even physical disks.
func TestStorageTargetSync_Startup_InMaintenanceDoesNotMountDisks(t *testing.T) {
	s := newTestStorageTargetSync(t)
	var diskCalls int32
	disk1 := storageTargetTestMount{where: "/mnt/disk1", calls: &diskCalls}
	scheduler := newTestScheduler(t)
	if err := scheduler.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}, Scheduler: scheduler}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	if atomic.LoadInt32(&diskCalls) != 0 {
		t.Fatalf("disk Mount() called %d times, want 0 while the array is in maintenance mode at startup", diskCalls)
	}
}

// TestStorageTargetSync_Startup_RegeneratesArrayMountsFromStore proves
// (#387) an array or a share created before
// ConditionPathExists=!disk.StorageStoppedFlagPath existed in
// disk.MountUnit.Render/pool.Mount.Render left its own unit
// condition-less forever, since array creation, a share mutation, and the
// disk add/replace/upgrade flows are the only other writers of those
// files — none of which a plain restart or a package upgrade of an
// otherwise untouched array ever runs again. Startup must rewrite every
// managed disk and pool mount unit from SQLite itself.
func TestStorageTargetSync_Startup_RegeneratesArrayMountsFromStore(t *testing.T) {
	ctx := context.Background()
	arrayStore, shareStore := newTestArrayAndShareStore(t)
	seedTestArray(t, ctx, arrayStore)

	generator := cfggen.NewGenerator(t.TempDir())
	diskUnitPath := "systemd/system/mnt-disk1.mount"
	if err := generator.Write(ctx, cfggen.File{Path: diskUnitPath, Command: "array create", Body: []byte("[Unit]\nDescription=stale\n\n[Mount]\nWhat=/dev/disk/by-uuid/aaaa\nWhere=/mnt/disk1\nType=xfs\n")}, 1, time.Now()); err != nil {
		t.Fatalf("seeding a pre-existing, condition-less disk mount unit: %v", err)
	}

	s := &storageTargetSync{
		Generator:       generator,
		Runner:          disk.NewFakeRunner(),
		FlagPath:        filepath.Join(t.TempDir(), "storage-ready"),
		StoppedFlagPath: filepath.Join(t.TempDir(), "array-stopped"),
		ArrayStore:      arrayStore,
		ShareStore:      shareStore,
		PoolMounted:     func(string) (bool, error) { return true, nil },
	}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}}

	if err := s.Startup(ctx, seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(generator.Root, diskUnitPath))
	if err != nil {
		t.Fatalf("reading %s: %v", diskUnitPath, err)
	}
	if !strings.Contains(string(got), "ConditionPathExists=!"+disk.StorageStoppedFlagPath) {
		t.Fatalf("%s = %q, want Startup to have rewritten it with the array-stopped condition (#387)", diskUnitPath, got)
	}
}

// TestStorageTargetSync_Startup_InMaintenanceLeavesGateClosed proves
// Startup's own maintenance-mode guard: a hoservad that starts up while
// Scheduler.InMaintenance() is already true — an explicit `array stop`
// still in force within this same process, or restored from SQLite across
// a restart (#387, RestorePersistedMaintenance) — must not mount the pool
// a user explicitly took down, even with every disk present.
func TestStorageTargetSync_Startup_InMaintenanceLeavesGateClosed(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	var catchAllCalls int32
	catchAll := storageTargetTestMount{where: "/mnt/user", calls: &catchAllCalls}
	scheduler := newTestScheduler(t)
	if err := scheduler.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, CatchAll: catchAll, Scheduler: scheduler}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	if atomic.LoadInt32(&catchAllCalls) != 0 {
		t.Fatalf("CatchAll.Mount() called %d times, want 0 while the array is in maintenance mode at startup", catchAllCalls)
	}
	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag exists even though the array is in maintenance mode at startup")
	}
	if _, err := os.Stat(s.stoppedFlagPath()); err != nil {
		t.Fatalf("Stat(the array-stopped flag) = %v, want it set from persisted maintenance state at startup (#387)", err)
	}
}

// TestStorageTargetSync_Startup_NotInMaintenanceClearsTheArrayStoppedFlag
// is the other half of #387's own regression: a fresh install, or one
// that has never run `array stop` (or already ran `array start`), must
// not leave every generated mount unit gated by a stale flag from before
// this boot.
func TestStorageTargetSync_Startup_NotInMaintenanceClearsTheArrayStoppedFlag(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	if err := os.MkdirAll(filepath.Dir(s.stoppedFlagPath()), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(s.stoppedFlagPath(), []byte("stopped\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	scheduler := newTestScheduler(t)
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Scheduler: scheduler}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if _, err := os.Stat(s.stoppedFlagPath()); err == nil {
		t.Fatal("the array-stopped flag still exists after a Startup that is not in maintenance")
	}
}

// TestStorageTargetSync_Startup_ClearsAStaleFlagFirst reproduces the exact
// data-loss path a stale on-disk artifact opened before this fix: with
// the flag left over from an earlier, ready boot, and this boot's own
// write refused (an unmanaged unit — the same refusal a hand edit
// already gets elsewhere in this package), Startup must still have
// removed the stale flag before attempting anything else, so a hoservad
// that then fails to finish this boot's own evaluation never leaves the
// earlier boot's "ready" in force.
func TestStorageTargetSync_Startup_ClearsAStaleFlagFirst(t *testing.T) {
	generator := cfggen.NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Now()
	readyUnitPath := "systemd/system/" + pool.StorageReadyUnitName
	if err := generator.Write(ctx, cfggen.File{Path: readyUnitPath, Command: "array status", Body: []byte("placeholder\n")}, 1, now); err != nil {
		t.Fatalf("seeding an existing managed file: %v", err)
	}
	if err := generator.KeepUnmanaged(ctx, readyUnitPath); err != nil {
		t.Fatalf("KeepUnmanaged: %v", err)
	}

	s := &storageTargetSync{
		Generator:       generator,
		Runner:          disk.NewFakeRunner(),
		FlagPath:        filepath.Join(t.TempDir(), "storage-ready"),
		StoppedFlagPath: filepath.Join(t.TempDir(), "array-stopped"),
	}
	if err := os.WriteFile(s.flagPath(), []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("seeding a stale flag: %v", err)
	}

	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}}
	if err := s.Startup(ctx, seq); err == nil {
		t.Fatal("Startup against an unmanaged ready unit = nil error, want one")
	}
	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the stale flag survived a Startup that failed after it should have been cleared first")
	}
}

// TestStorageTargetSync_Startup_FailedWriteLeavesGateClosed is finding
// 3's "second path": a refused unit write must never leave a stale
// "ready" state in force, and must be reported as an error so main.go
// knows not to send systemd READY=1 over it.
func TestStorageTargetSync_Startup_FailedWriteLeavesGateClosed(t *testing.T) {
	generator := cfggen.NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Now()
	readyUnitPath := "systemd/system/" + pool.StorageReadyUnitName
	if err := generator.Write(ctx, cfggen.File{Path: readyUnitPath, Command: "array status", Body: []byte("placeholder\n")}, 1, now); err != nil {
		t.Fatalf("seeding an existing managed file: %v", err)
	}
	if err := generator.KeepUnmanaged(ctx, readyUnitPath); err != nil {
		t.Fatalf("KeepUnmanaged: %v", err)
	}

	s := &storageTargetSync{
		Generator:       generator,
		Runner:          disk.NewFakeRunner(),
		FlagPath:        filepath.Join(t.TempDir(), "storage-ready"),
		StoppedFlagPath: filepath.Join(t.TempDir(), "array-stopped"),
	}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}}

	if err := s.Startup(ctx, seq); err == nil {
		t.Fatal("Startup with an unmanaged ready unit = nil error, want one")
	}
	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag exists after a failed unit write — the gate must stay closed")
	}
}

// TestStorageTargetSync_Update_UnchangedIssuesNoSystemctlCall is finding
// 1's own regression test: a rebuild whose readiness and disk topology
// have not actually changed (a share create, say) must never touch
// systemd at all — never mind restart, not even a daemon-reload.
func TestStorageTargetSync_Update_UnchangedIssuesNoSystemctlCall(t *testing.T) {
	s := newTestStorageTargetSync(t)
	seq := &job.ArraySequence{
		Gate:  storageTargetTestGate{ready: true},
		Disks: []job.ArrayMount{storageTargetTestMount{where: "/mnt/disk1"}},
	}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	s.Runner = disk.NewFakeRunner()

	s.Update(context.Background(), seq)

	fakeRunner := s.Runner.(*disk.FakeRunner)
	if calls := fakeRunner.Calls(); len(calls) != 0 {
		t.Fatalf("Calls() = %v, want zero systemctl calls for an unchanged rebuild", calls)
	}
}

// TestStorageTargetSync_Update_NeverRestarts proves (#372) the actual
// mechanism, not just its symptom: even across a real transition, Update
// never issues "systemctl restart" of anything — systemd's BindsTo=/
// Requires= (pool.ServiceDropIn) would propagate a restart of
// hoserva-storage-ready.service straight through to every dependent
// service, stopping and restarting Samba, NFS, Docker and libvirt even
// though they were already running correctly.
func TestStorageTargetSync_Update_NeverRestarts(t *testing.T) {
	s := newTestStorageTargetSync(t)
	notReady := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}}
	ready := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}}

	if err := s.Startup(context.Background(), notReady); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	s.Runner = disk.NewFakeRunner()

	s.Update(context.Background(), ready)

	fakeRunner := s.Runner.(*disk.FakeRunner)
	if hasCallWithVerb(fakeRunner.Calls(), "restart") {
		t.Fatalf("Calls() = %v, want no systemctl restart from Update", fakeRunner.Calls())
	}
}

// TestStorageTargetSync_Update_NotReadyToReadyStartsDependents is finding
// 2's regression test: a live not-ready→ready flip must bring Samba, NFS,
// Docker and libvirt back itself, by starting every unit in
// pool.DependentServiceUnits directly — confirmed against a real systemd
// (session 372-a1's own lab) that starting hoserva-storage.target alone
// does *not* do this: pool.ServiceDropIn's BindsTo=hoserva-storage.target
// lives on each dependent's own drop-in, so only starting a dependent —
// never the target on its own — pulls the relationship the other way.
// "start" on an already-active unit is a no-op either way (also confirmed
// there), so this is safe to issue unconditionally on the transition.
func TestStorageTargetSync_Update_NotReadyToReadyStartsDependents(t *testing.T) {
	s := newTestStorageTargetSync(t)
	notReady := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}}
	ready := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}}

	if err := s.Startup(context.Background(), notReady); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag exists while the gate refuses readiness")
	}
	s.Runner = disk.NewFakeRunner()

	s.Update(context.Background(), ready)

	if _, err := os.Stat(s.flagPath()); err != nil {
		t.Fatalf("Stat(flag) = %v, want the flag written once the gate flips ready", err)
	}
	fakeRunner := s.Runner.(*disk.FakeRunner)
	for _, svc := range pool.DependentServiceUnits {
		if !hasCall(fakeRunner.Calls(), "systemctl", "start", svc) {
			t.Fatalf("Calls() = %v, want a systemctl start %s on a not-ready→ready transition", fakeRunner.Calls(), svc)
		}
	}
}

// TestStorageTargetSync_Update_OneMissingDependentDoesNotBlockTheOthers
// proves the not-ready→ready transition survives Docker or libvirt not
// being installed at all (D8, doc 14): each dependent is started
// independently, so a "unit not found" for one must never stop smbd or
// nfs-kernel-server from starting, and must never leave this bookkeeping
// stuck treating the transition as still pending on every future rebuild.
func TestStorageTargetSync_Update_OneMissingDependentDoesNotBlockTheOthers(t *testing.T) {
	s := newTestStorageTargetSync(t)
	runner := disk.NewFakeRunner()
	runner.Script("systemctl", []string{"start", "docker.service"}, nil, errors.New("Unit docker.service not found."))
	s.Runner = runner
	notReady := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}}
	ready := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}}

	if err := s.Startup(context.Background(), notReady); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	s.Update(context.Background(), ready)

	fakeRunner := s.Runner.(*disk.FakeRunner)
	if !hasCall(fakeRunner.Calls(), "systemctl", "start", "smbd.service") {
		t.Fatalf("Calls() = %v, want smbd.service started despite docker.service failing", fakeRunner.Calls())
	}
	if !s.ready || !s.applied {
		t.Fatalf("ready=%v applied=%v, want both true — a missing optional dependent must not leave the transition stuck pending", s.ready, s.applied)
	}

	// A later, unrelated Update call with the same seq must issue no
	// further systemctl calls at all: the transition already completed.
	s.Runner = disk.NewFakeRunner()
	s.Update(context.Background(), ready)
	if calls := s.Runner.(*disk.FakeRunner).Calls(); len(calls) != 0 {
		t.Fatalf("Calls() = %v, want zero systemctl calls once the transition is already applied", calls)
	}
}

// TestStorageTargetSync_Update_ReadyToNotReadyOnlyClearsFlag proves the
// other half of Update's own contract: going degraded live only removes
// the flag — it must never stop a service that is already correctly
// running (doc 02 §4's own explicit array-stop sequence is what stops
// them, not this gate).
func TestStorageTargetSync_Update_ReadyToNotReadyOnlyClearsFlag(t *testing.T) {
	s := newTestStorageTargetSync(t)
	ready := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}}
	notReady := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}}

	if err := s.Startup(context.Background(), ready); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	s.Runner = disk.NewFakeRunner()

	s.Update(context.Background(), notReady)

	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag survived a ready→not-ready transition")
	}
	fakeRunner := s.Runner.(*disk.FakeRunner)
	if hasCallWithVerb(fakeRunner.Calls(), "start", "stop", "restart") {
		t.Fatalf("Calls() = %v, want no start/stop/restart on a ready→not-ready transition", fakeRunner.Calls())
	}
}

// TestStorageTargetSync_Update_TopologyChangeReloadsWithoutStartOrRestart
// proves a disk topology change alone (readiness unchanged) rewrites the
// target's own soft Wants=/After= ordering and reloads systemd, but still
// never starts or restarts anything — the running target's own active
// state is untouched by new file content until something else
// legitimately starts it.
func TestStorageTargetSync_Update_TopologyChangeReloadsWithoutStartOrRestart(t *testing.T) {
	s := newTestStorageTargetSync(t)
	before := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{storageTargetTestMount{where: "/mnt/disk1"}}}
	after := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{storageTargetTestMount{where: "/mnt/disk1"}, storageTargetTestMount{where: "/mnt/disk2"}}}

	if err := s.Startup(context.Background(), before); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	s.Runner = disk.NewFakeRunner()

	s.Update(context.Background(), after)

	fakeRunner := s.Runner.(*disk.FakeRunner)
	if !hasCall(fakeRunner.Calls(), "systemctl", "daemon-reload") {
		t.Fatalf("Calls() = %v, want a daemon-reload once the disk topology changes", fakeRunner.Calls())
	}
	if hasCallWithVerb(fakeRunner.Calls(), "start", "restart") {
		t.Fatalf("Calls() = %v, want no start/restart from a topology-only change", fakeRunner.Calls())
	}
	got, err := os.ReadFile(filepath.Join(s.Generator.Root, "systemd", "system", pool.StorageTargetUnitName))
	if err != nil {
		t.Fatalf("reading %s: %v", pool.StorageTargetUnitName, err)
	}
	if !strings.Contains(string(got), disk.UnitFileName("/mnt/disk2")) {
		t.Fatalf("%s = %q, want it to list the newly added disk", pool.StorageTargetUnitName, got)
	}
}

// TestStorageTargetSync_Update_NotReadyToReadyMountsPoolBeforeFlagAndStart
// is the ordering this issue exists to prove: disk.StorageGate.Ready()
// only reports every expected disk present by identity, never whether the
// pool that serves them is mounted, and a physical disk mount returning
// does not pull mergerfs's own catch-all mount up with it. The catch-all
// and every share mount must actually be mounted, and confirmed mounted,
// before the readiness flag is written and before any dependent starts —
// never a client writing straight into an unmounted /mnt/user just
// because every unit this gate installs happens to report active.
func TestStorageTargetSync_Update_NotReadyToReadyMountsPoolBeforeFlagAndStart(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	var catchAllCalls, shareCalls int32
	catchAll := storageTargetTestMount{where: "/mnt/user", calls: &catchAllCalls}
	shareMount := storageTargetTestMount{where: "/mnt/user/massdel", calls: &shareCalls}
	notReady := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}, CatchAll: catchAll, ShareMounts: []job.ArrayMount{shareMount}}
	ready := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, CatchAll: catchAll, ShareMounts: []job.ArrayMount{shareMount}}

	if err := s.Startup(context.Background(), notReady); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	s.Runner = disk.NewFakeRunner()

	s.Update(context.Background(), ready)

	if atomic.LoadInt32(&catchAllCalls) != 1 {
		t.Fatalf("catch-all Mount() called %d times, want exactly 1", catchAllCalls)
	}
	if atomic.LoadInt32(&shareCalls) != 1 {
		t.Fatalf("share Mount() called %d times, want exactly 1", shareCalls)
	}
	if _, err := os.Stat(s.flagPath()); err != nil {
		t.Fatalf("Stat(flag) = %v, want the flag written once the pool is confirmed mounted", err)
	}
	fakeRunner := s.Runner.(*disk.FakeRunner)
	for _, svc := range pool.DependentServiceUnits {
		if !hasCall(fakeRunner.Calls(), "systemctl", "start", svc) {
			t.Fatalf("Calls() = %v, want a systemctl start %s once the pool is confirmed mounted", fakeRunner.Calls(), svc)
		}
	}
}

// TestStorageTargetSync_Update_MountFailureLeavesGateClosed proves a
// failed catch-all mount is treated exactly like the gate itself
// reporting not ready: no flag, no dependent start, and — since s.ready
// never flips to true — the next rebuild retries the same transition
// instead of it being lost.
func TestStorageTargetSync_Update_MountFailureLeavesGateClosed(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	catchAll := storageTargetTestMount{where: "/mnt/user", mountErr: errors.New("mount: /mnt/user: special device none does not exist")}
	notReady := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}, CatchAll: catchAll}
	ready := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, CatchAll: catchAll}

	if err := s.Startup(context.Background(), notReady); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	s.Runner = disk.NewFakeRunner()

	s.Update(context.Background(), ready)

	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag exists even though the pool failed to mount")
	}
	fakeRunner := s.Runner.(*disk.FakeRunner)
	if hasCallWithVerb(fakeRunner.Calls(), "start") {
		t.Fatalf("Calls() = %v, want no dependent started while the pool failed to mount", fakeRunner.Calls())
	}
}

// TestStorageTargetSync_Update_UnconfirmedMountLeavesGateClosed proves
// the same for a Mount() call that returns success but does not actually
// leave the catch-all mounted (PoolMounted reports false) — the mount
// call succeeding is not enough on its own; this issue exists because a
// unit reporting active was already shown not to be enough either.
func TestStorageTargetSync_Update_UnconfirmedMountLeavesGateClosed(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return false, nil }
	catchAll := storageTargetTestMount{where: "/mnt/user"}
	notReady := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}, CatchAll: catchAll}
	ready := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, CatchAll: catchAll}

	if err := s.Startup(context.Background(), notReady); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	s.Runner = disk.NewFakeRunner()

	s.Update(context.Background(), ready)

	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag exists even though PoolMounted reported the catch-all not actually mounted")
	}
	fakeRunner := s.Runner.(*disk.FakeRunner)
	if hasCallWithVerb(fakeRunner.Calls(), "start") {
		t.Fatalf("Calls() = %v, want no dependent started while the pool is not confirmed mounted", fakeRunner.Calls())
	}
}

// TestStorageTargetSync_Update_InMaintenanceLeavesGateClosed proves a
// not-ready→ready transition during an explicit `array stop` (doc 02 §4,
// Q70) never mounts or serves anything: ArraySequence.Stop already
// stopped Samba/NFS directly for that, and an in-progress disk swap is
// exactly the case where every other expected disk staying present must
// not be read as "bring the array back".
func TestStorageTargetSync_Update_InMaintenanceLeavesGateClosed(t *testing.T) {
	s := newTestStorageTargetSync(t)
	var catchAllCalls int32
	catchAll := storageTargetTestMount{where: "/mnt/user", calls: &catchAllCalls}
	scheduler := newTestScheduler(t)
	if err := scheduler.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	notReady := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}, CatchAll: catchAll, Scheduler: scheduler}
	ready := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, CatchAll: catchAll, Scheduler: scheduler}

	if err := s.Startup(context.Background(), notReady); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	s.Runner = disk.NewFakeRunner()

	s.Update(context.Background(), ready)

	if atomic.LoadInt32(&catchAllCalls) != 0 {
		t.Fatalf("catch-all Mount() called %d times, want 0 while the array is in maintenance mode", catchAllCalls)
	}
	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag exists even though the array is in maintenance mode")
	}
	fakeRunner := s.Runner.(*disk.FakeRunner)
	if hasCallWithVerb(fakeRunner.Calls(), "start") {
		t.Fatalf("Calls() = %v, want no dependent started while the array is in maintenance mode", fakeRunner.Calls())
	}
}

// TestStorageTargetSync_Update_DisabledDependentIsNotStarted proves
// (#372) an admin who disabled a dependent on
// purpose (no VMs or Apps, SMB-only sharing, ...) must not have it started
// back up just because the storage gate opened. Without startDependents
// reusing disk.ServiceUnitController's own LoadState/UnitFileState rule,
// Update issues a raw "systemctl start" against every one of
// pool.DependentServiceUnits regardless.
func TestStorageTargetSync_Update_DisabledDependentIsNotStarted(t *testing.T) {
	s := newTestStorageTargetSync(t)
	runner := disk.NewFakeRunner()
	runner.Script("systemctl", []string{"show", "--property=LoadState", "--value", "docker.service"}, []byte("loaded"), nil)
	runner.Script("systemctl", []string{"show", "--property=UnitFileState", "--value", "docker.service"}, []byte("disabled"), nil)
	s.Runner = runner
	notReady := &job.ArraySequence{Gate: storageTargetTestGate{ready: false}}
	ready := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}}

	if err := s.Startup(context.Background(), notReady); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	s.Update(context.Background(), ready)

	fakeRunner := s.Runner.(*disk.FakeRunner)
	if hasCall(fakeRunner.Calls(), "systemctl", "start", "docker.service") {
		t.Fatalf("Calls() = %v, want docker.service never started — it was disabled on purpose", fakeRunner.Calls())
	}
	if !hasCall(fakeRunner.Calls(), "systemctl", "start", "smbd.service") {
		t.Fatalf("Calls() = %v, want smbd.service started despite docker.service being disabled", fakeRunner.Calls())
	}
}

// TestStorageTargetSync_UpdateOrError_FailedWriteIsReported is #388's own
// regression test for finding 3 (raised by the #385 executor): a refused
// unit-file write (a hand-edited managed unit) must reach UpdateOrError's
// own caller as an error — #385's AcknowledgeDegraded hook needs exactly
// this to tell the acknowledge operation the transition did not actually
// take effect — never be logged and reported as success with stale
// boot-ordering units left on disk. Update itself keeps only logging it
// (TestStorageTargetSync_Update_TopologyChangeReloadsWithoutStartOrRestart
// and friends already cover Update's own callers never seeing a panic or
// crash from this), since it wraps the same updateTransition call.
func TestStorageTargetSync_UpdateOrError_FailedWriteIsReported(t *testing.T) {
	generator := cfggen.NewGenerator(t.TempDir())
	ctx := context.Background()
	now := time.Now()
	readyUnitPath := "systemd/system/" + pool.StorageReadyUnitName
	if err := generator.Write(ctx, cfggen.File{Path: readyUnitPath, Command: "array status", Body: []byte("placeholder\n")}, 1, now); err != nil {
		t.Fatalf("seeding an existing managed file: %v", err)
	}
	if err := generator.KeepUnmanaged(ctx, readyUnitPath); err != nil {
		t.Fatalf("KeepUnmanaged: %v", err)
	}

	s := &storageTargetSync{
		Generator:       generator,
		Runner:          disk.NewFakeRunner(),
		FlagPath:        filepath.Join(t.TempDir(), "storage-ready"),
		StoppedFlagPath: filepath.Join(t.TempDir(), "array-stopped"),
	}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}}

	if err := s.UpdateOrError(ctx, seq); err == nil {
		t.Fatal("UpdateOrError with an unmanaged ready unit = nil error, want one")
	}
}

// TestStorageTargetSync_ConfirmReady_SetsFlagAndStartsServices proves
// (#372) the explicit `array start` path:
// job.ArraySequence.Start calls ConfirmReady once its own mounts are up,
// and ConfirmReady must write the units, confirm the pool, set the
// readiness flag and start every eligible dependent — the same
// transition Update makes for a live not-ready→ready flip.
func TestStorageTargetSync_ConfirmReady_SetsFlagAndStartsServices(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	seq := &job.ArraySequence{CatchAll: storageTargetTestMount{where: "/mnt/user"}}

	if err := s.ConfirmReady(context.Background(), seq); err != nil {
		t.Fatalf("ConfirmReady: %v", err)
	}

	if _, err := os.Stat(s.flagPath()); err != nil {
		t.Fatalf("Stat(flag) = %v, want the flag written once ConfirmReady succeeds", err)
	}
	fakeRunner := s.Runner.(*disk.FakeRunner)
	for _, svc := range pool.DependentServiceUnits {
		if !hasCall(fakeRunner.Calls(), "systemctl", "start", svc) {
			t.Fatalf("Calls() = %v, want a systemctl start %s from ConfirmReady", fakeRunner.Calls(), svc)
		}
	}
}

// TestStorageTargetSync_ConfirmReady_RunsDuringMaintenance proves
// ConfirmReady never defers to maintenance mode the way Update does —
// job.ArraySequence.Start's own caller is still nominally "in
// maintenance" at the point Start calls this (Start only exits
// maintenance once every one of its own steps, including this one, has
// succeeded), so treating that as a reason to leave the gate closed would
// make an explicit `array start` after a not-ready boot (doc 02 §4 E6)
// never actually bring Samba/NFS back.
func TestStorageTargetSync_ConfirmReady_RunsDuringMaintenance(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	scheduler := newTestScheduler(t)
	if err := scheduler.EnterMaintenance(context.Background()); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	seq := &job.ArraySequence{CatchAll: storageTargetTestMount{where: "/mnt/user"}, Scheduler: scheduler}

	if err := s.ConfirmReady(context.Background(), seq); err != nil {
		t.Fatalf("ConfirmReady: %v", err)
	}

	if _, err := os.Stat(s.flagPath()); err != nil {
		t.Fatalf("Stat(flag) = %v, want the flag written even while the array is still in maintenance mode", err)
	}
}

// TestStorageTargetSync_ConfirmReady_UnconfirmedMountReturnsError proves
// ConfirmReady never mounts the pool itself and never sets the flag over
// an unconfirmed one: job.ArraySequence.Start's own Disks/CatchAll/
// ShareMounts loops, immediately before this call, already mounted
// everything — if that somehow left the catch-all not actually mounted,
// this must fail rather than starting Samba/NFS over it.
func TestStorageTargetSync_ConfirmReady_UnconfirmedMountReturnsError(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return false, nil }
	var mountCalls int32
	seq := &job.ArraySequence{CatchAll: storageTargetTestMount{where: "/mnt/user", calls: &mountCalls}}

	if err := s.ConfirmReady(context.Background(), seq); err == nil {
		t.Fatal("ConfirmReady with an unconfirmed pool mount = nil error, want one")
	}
	if atomic.LoadInt32(&mountCalls) != 0 {
		t.Fatalf("CatchAll.Mount() called %d times, want 0 — ConfirmReady confirms, it never mounts", mountCalls)
	}
	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag exists even though the pool never confirmed mounted")
	}
	fakeRunner := s.Runner.(*disk.FakeRunner)
	if hasCallWithVerb(fakeRunner.Calls(), "start") {
		t.Fatalf("Calls() = %v, want no dependent started while the pool is not confirmed mounted", fakeRunner.Calls())
	}
}

// TestStorageTargetSync_Startup_PoolNotMountedLeavesGateClosed proves the
// disk-returned boot's own contract: the ready unit's flag must never
// exist on a boot where every expected disk is present but the pool they
// serve never actually mounted — Startup returns an error in that case
// (so main.go never sends systemd READY=1 over it, matching every other
// Startup failure).
func TestStorageTargetSync_Startup_PoolNotMountedLeavesGateClosed(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return false, nil }
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, CatchAll: storageTargetTestMount{where: "/mnt/user"}}

	if err := s.Startup(context.Background(), seq); err == nil {
		t.Fatal("Startup with an unconfirmed pool mount = nil error, want one")
	}
	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag exists even though the pool never confirmed mounted")
	}
}

// blockingUntilCancelledMount is job.ArrayMount's own fake for
// TestStorageTargetSync_Startup_BoundsAMountThatNeverReturns (#388): Mount
// never returns on its own, the way `systemctl start` on a mount unit
// whose device dependency never resolves never returns on its own either
// — it only returns once ctx is cancelled, mirroring exec.CommandContext
// killing the real systemctl client the instant startupMountTimeout
// expires.
type blockingUntilCancelledMount struct{ where string }

func (m blockingUntilCancelledMount) Where() string { return m.where }
func (m blockingUntilCancelledMount) Mount(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
func (m blockingUntilCancelledMount) Unmount(context.Context) error { return nil }

// TestStorageTargetSync_Startup_BoundsAMountThatNeverReturns is #388's own
// regression test for the restart-loop report: a mount that never returns
// on its own (a replacement disk with the right serial but the wrong
// filesystem, or any other device whose mount unit never activates) must
// not block Startup past its own bound — otherwise hoservad never reaches
// main.go's unconditional notifySystemdReady call, and systemd's
// Type=notify TimeoutStartSec kills the whole process before it ever
// sends READY=1, restarting forever. StartupMountTimeout is set well
// below the real 90s default so this test proves the bound is enforced
// without actually waiting anywhere near it.
func TestStorageTargetSync_Startup_BoundsAMountThatNeverReturns(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.StartupMountTimeout = 50 * time.Millisecond
	disk1 := blockingUntilCancelledMount{where: "/mnt/disk1"}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}}

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- s.Startup(context.Background(), seq) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Startup with a mount that never returns = nil error, want one")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("Startup took %v, want it bounded near StartupMountTimeout (%v)", elapsed, s.StartupMountTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Startup did not return within 5s of a mount that never returns on its own — it would block main.go's own READY=1 forever")
	}
	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag exists even though the mount never confirmed")
	}
}

// TestStorageTargetSync_Startup_BoundsAShareMountThatNeverReturns proves
// the same bound covers a per-share mergerfs mount
// (run-hoserva-array-<share>.mount, the orchestrator's own nightly L3 run
// 36243538178 finding on an *ordinary* reboot with every disk present:
// hoserva.service was still "activating (start)" 4s in, its child process
// `systemctl start run-hoserva-array-hoserval3midsync.mount`), not just a
// physical disk mount — mountAndConfirmPool (which mounts the catch-all,
// then every ShareMounts entry, mountPool's own doc comment) runs inside
// the exact same mountCtx Startup already bounds mountArrayDisks with, so
// a share mount that never returns is bounded identically. CatchAll is a
// fast, succeeding mount here — mountAndConfirmPool returns immediately
// without ever calling mountPool when CatchAll is nil, which would prove
// nothing about ShareMounts's own bound — so this isolates that the
// blocking share mount, not the catch-all, is what gets bounded.
func TestStorageTargetSync_Startup_BoundsAShareMountThatNeverReturns(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.StartupMountTimeout = 50 * time.Millisecond
	catchAll := storageTargetTestMount{where: "/mnt/user"}
	share := blockingUntilCancelledMount{where: "/mnt/user/massdel"}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, CatchAll: catchAll, ShareMounts: []job.ArrayMount{share}}

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- s.Startup(context.Background(), seq) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Startup with a share mount that never returns = nil error, want one")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("Startup took %v, want it bounded near StartupMountTimeout (%v)", elapsed, s.StartupMountTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Startup did not return within 5s of a share mount that never returns on its own — it would block main.go's own READY=1 forever")
	}
	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag exists even though the share mount never confirmed")
	}
}

// TestStorageTargetSync_Close_StopsGateAndDockerAndLibvirtAndClearsFlag is
// #387's own regression for the storage-target gate closing on `array
// stop` (#372): hoserva-storage-ready.service is
// RemainAfterExit=yes, so ArraySequence.Stop's own Services loop stopping
// Samba and NFS directly never touches it — left alone, it stays
// "active (exited)" and anything that later starts Samba or NFS during
// maintenance still passes hoserva-storage.target. Close must stop it
// (so its fixed `test -e` reruns — and fails — next time), stop Docker
// and libvirt (doc 02 §1, Q70: neither is in ArraySequence's own Services
// list, #309), and remove the flag.
func TestStorageTargetSync_Close_StopsGateAndDockerAndLibvirtAndClearsFlag(t *testing.T) {
	s := newTestStorageTargetSync(t)
	if err := os.MkdirAll(filepath.Dir(s.flagPath()), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(s.flagPath(), []byte("ready\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := s.Close(context.Background(), true); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag still exists after Close")
	}
	if _, err := os.Stat(s.stoppedFlagPath()); err != nil {
		t.Fatalf("Stat(the array-stopped flag) = %v, want it created by Close (#387)", err)
	}
	fakeRunner := s.Runner.(*disk.FakeRunner)
	if !hasCall(fakeRunner.Calls(), "systemctl", "stop", "docker.service") {
		t.Fatalf("Calls() = %v, want a systemctl stop docker.service from Close", fakeRunner.Calls())
	}
	if !hasCall(fakeRunner.Calls(), "systemctl", "stop", "libvirtd.service") {
		t.Fatalf("Calls() = %v, want a systemctl stop libvirtd.service from Close", fakeRunner.Calls())
	}
	if !hasCall(fakeRunner.Calls(), "systemctl", "stop", pool.StorageReadyUnitName) {
		t.Fatalf("Calls() = %v, want a systemctl stop %s from Close", fakeRunner.Calls(), pool.StorageReadyUnitName)
	}
	if s.Ready() {
		t.Fatal("Ready() = true after Close")
	}
}

// TestStorageTargetSync_Close_TransientNeverSetsTheStoppedFlag proves
// (#387) Close(ctx, false) is StopForShutdown's own
// call (a reboot or a UPS low-battery shutdown) — never a user asking the
// array to stay stopped — so it must never create the durable
// array-stopped condition flag, even though it still stops Docker,
// libvirt and the gate unit exactly like a persisted close. A flag left
// behind here would fail every mount unit's own ConditionPathExists in
// the boot-time systemd transaction that runs before hoservad is even
// exec'd, stranding the array unmounted after an ordinary reboot.
func TestStorageTargetSync_Close_TransientNeverSetsTheStoppedFlag(t *testing.T) {
	s := newTestStorageTargetSync(t)

	if err := s.Close(context.Background(), false); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(s.stoppedFlagPath()); err == nil {
		t.Fatal("the array-stopped flag exists after a transient Close (persist=false) — a plain reboot must not strand the array unmounted")
	}
	fakeRunner := s.Runner.(*disk.FakeRunner)
	if !hasCall(fakeRunner.Calls(), "systemctl", "stop", "docker.service") {
		t.Fatalf("Calls() = %v, want a systemctl stop docker.service from a transient Close too", fakeRunner.Calls())
	}
	if !hasCall(fakeRunner.Calls(), "systemctl", "stop", pool.StorageReadyUnitName) {
		t.Fatalf("Calls() = %v, want a systemctl stop %s from a transient Close too", fakeRunner.Calls(), pool.StorageReadyUnitName)
	}
}

// TestStorageTargetSync_Close_TransientLeavesAnExistingStoppedFlagInPlace
// proves the other half of #387: a persisted user `array stop`
// already in force before a later reboot or UPS shutdown must survive it
// — Close(ctx, false) must neither create nor clear the flag, only leave
// it exactly as the persisted row already left it.
func TestStorageTargetSync_Close_TransientLeavesAnExistingStoppedFlagInPlace(t *testing.T) {
	s := newTestStorageTargetSync(t)
	if err := os.MkdirAll(filepath.Dir(s.stoppedFlagPath()), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(s.stoppedFlagPath(), []byte("stopped\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := s.Close(context.Background(), false); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(s.stoppedFlagPath()); err != nil {
		t.Fatalf("Stat(the array-stopped flag) = %v, want a flag already set by an earlier persisted stop to survive a transient Close", err)
	}
}

// TestStorageTargetSync_Open_RemovesTheArrayStoppedFlag is #387's own
// regression: every generated mount unit now carries ConditionPathExists=
// against this flag, so Open (job.ArraySequence.Start's own first call on
// StorageTarget) must remove it before hoservad's own first Mount call —
// without this, that Mount would find the condition unmet and silently
// skip, exactly like the external starts the flag exists to block.
func TestStorageTargetSync_Open_RemovesTheArrayStoppedFlag(t *testing.T) {
	s := newTestStorageTargetSync(t)
	if err := os.MkdirAll(filepath.Dir(s.stoppedFlagPath()), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(s.stoppedFlagPath(), []byte("stopped\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := s.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(s.stoppedFlagPath()); err == nil {
		t.Fatal("the array-stopped flag still exists after Open")
	}
}

// TestStorageTargetSync_Open_AbsentFlagIsSuccess proves Open is
// idempotent: an array that was never stopped this way (or already
// started) has nothing to remove, and that is success, not an error —
// the same convention clearFlag already follows for the readiness flag.
func TestStorageTargetSync_Open_AbsentFlagIsSuccess(t *testing.T) {
	s := newTestStorageTargetSync(t)
	if err := s.Open(context.Background()); err != nil {
		t.Fatalf("Open with no flag present: %v", err)
	}
}

// TestStorageTargetSync_Close_FailedDependentStopAbortsBeforeClosingTheGate
// proves a Docker or libvirt stop failure — a container still holding a
// file open on the pool is doc 02 §4's own example — aborts Close before
// it ever touches the gate unit or the readiness flag: leaving the gate itself
// closed while a service that never actually stopped is still running
// would be reporting a stop that did not fully happen.
func TestStorageTargetSync_Close_FailedDependentStopAbortsBeforeClosingTheGate(t *testing.T) {
	s := newTestStorageTargetSync(t)
	if err := os.MkdirAll(filepath.Dir(s.flagPath()), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(s.flagPath(), []byte("ready\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fakeRunner := s.Runner.(*disk.FakeRunner)
	fakeRunner.Script("systemctl", []string{"stop", "docker.service"}, nil, errors.New("docker.service: still stopping"))

	if err := s.Close(context.Background(), true); err == nil {
		t.Fatal("Close with a failed docker.service stop = nil error, want one")
	}
	if _, err := os.Stat(s.flagPath()); err != nil {
		t.Fatalf("Stat(flag) = %v, want the flag left in place after a failed Close", err)
	}
	if hasCall(fakeRunner.Calls(), "systemctl", "stop", pool.StorageReadyUnitName) {
		t.Fatal("Close stopped the gate unit even though docker.service refused to stop first")
	}
}

// flagCheckRunner wraps a disk.Runner and records, the moment name/args
// is run, whether flagPath still existed at that instant — this file's
// own probe for #387's ordering: a start landing between
// removing the flag and stopping the gate unit must find the flag already
// gone, not the reverse.
type flagCheckRunner struct {
	disk.Runner
	flagPath   string
	name       string
	args       []string
	ran        *bool
	flagExists *bool
}

func (r flagCheckRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == r.name && equalStringSlices(args, r.args) {
		*r.ran = true
		if _, err := os.Stat(r.flagPath); err == nil {
			*r.flagExists = true
		}
	}
	return r.Runner.Run(ctx, name, args...)
}

// TestStorageTargetSync_Close_RemovesFlagBeforeStoppingTheGateUnit proves
// (#387) a Samba or NFS start racing Close (an
// unattended-upgrades restart, or a manual `systemctl start`) must land
// on a `test -e` that already fails, not on a flag Close has not gotten
// around to removing yet. Close must therefore remove the flag before it
// stops hoserva-storage-ready.service, never after.
func TestStorageTargetSync_Close_RemovesFlagBeforeStoppingTheGateUnit(t *testing.T) {
	s := newTestStorageTargetSync(t)
	if err := os.MkdirAll(filepath.Dir(s.flagPath()), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(s.flagPath(), []byte("ready\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var ran, flagExists bool
	s.Runner = flagCheckRunner{
		Runner:     s.Runner,
		flagPath:   s.flagPath(),
		name:       "systemctl",
		args:       []string{"stop", pool.StorageReadyUnitName},
		ran:        &ran,
		flagExists: &flagExists,
	}

	if err := s.Close(context.Background(), true); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !ran {
		t.Fatal("Close never stopped the gate unit")
	}
	if flagExists {
		t.Fatal("the readiness flag still existed when Close stopped the gate unit — a start landing exactly here would still find it present and pass hoserva-storage.target (#387)")
	}
}

// TestStorageTargetSync_Startup_RecordsAFailedDiskMount is #398's own
// signal: a disk present by identity — the gate itself reports ready,
// since #388's own fix never treats an unknown filesystem as a mismatch
// — whose own mount call fails must have its mountpoint recorded, so
// api.Handler.MountFailedSlots (wired to this method in main.go) can tell
// GetPool which present slot actually never mounted.
func TestStorageTargetSync_Startup_RecordsAFailedDiskMount(t *testing.T) {
	s := newTestStorageTargetSync(t)
	disk1 := storageTargetTestMount{where: "/mnt/disk1", mountErr: errors.New("device dependency never resolved")}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}}

	if err := s.Startup(context.Background(), seq); err == nil {
		t.Fatal("Startup with a failing disk mount = nil error, want one")
	}

	failed := s.MountFailedMountpoints()
	if !failed["/mnt/disk1"] {
		t.Fatalf("MountFailedMountpoints() = %v, want /mnt/disk1 recorded", failed)
	}
}

// TestStorageTargetSync_Startup_NeverRecordsAMountpointThatSucceeded is
// the same test's negative half: an ordinary, successful mount records
// nothing — MountFailedMountpoints must never carry a slot that is
// genuinely serving.
func TestStorageTargetSync_Startup_NeverRecordsAMountpointThatSucceeded(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.PoolMounted = func(string) (bool, error) { return true, nil }
	disk1 := storageTargetTestMount{where: "/mnt/disk1"}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	if failed := s.MountFailedMountpoints(); len(failed) != 0 {
		t.Fatalf("MountFailedMountpoints() = %v, want none after a successful mount", failed)
	}
}

// TestStorageTargetSync_Startup_ReconcilesAMountpointThatIsNowMounted
// proves the self-healing half of #398: a mountpoint left over from an
// earlier failed attempt (as if a previous boot had recorded it) is
// dropped once the live mount table (s.poolMounted, overridden here)
// reports it genuinely mounted — a slot must not stay "needs attention"
// forever just because the flag this sync tracks in memory was never
// cleared by hand.
func TestStorageTargetSync_Startup_ReconcilesAMountpointThatIsNowMounted(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.mountFailedMountpoints = map[string]bool{"/mnt/disk1": true}
	s.PoolMounted = func(path string) (bool, error) { return path == "/mnt/disk1", nil }
	disk1 := storageTargetTestMount{where: "/mnt/disk1"}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}}

	if err := s.Startup(context.Background(), seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	if failed := s.MountFailedMountpoints(); len(failed) != 0 {
		t.Fatalf("MountFailedMountpoints() = %v, want empty — /mnt/disk1 is now genuinely mounted", failed)
	}
}

// TestStorageTargetSync_Update_ReconcilesAMountpointThatIsNowMounted is
// the same self-healing proof on the rebuild path (the one a successful
// disk_replace job's own topologyChanged hook actually reaches, unlike
// Startup, which only ever runs once at boot): a mountpoint recorded
// failed earlier in this process's own lifetime must clear on the next
// rebuild once it is genuinely mounted, without waiting for a restart.
func TestStorageTargetSync_Update_ReconcilesAMountpointThatIsNowMounted(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.mountFailedMountpoints = map[string]bool{"/mnt/disk1": true}
	s.PoolMounted = func(path string) (bool, error) { return path == "/mnt/disk1", nil }
	disk1 := storageTargetTestMount{where: "/mnt/disk1"}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}}

	s.Update(context.Background(), seq)

	if failed := s.MountFailedMountpoints(); len(failed) != 0 {
		t.Fatalf("MountFailedMountpoints() = %v, want empty — /mnt/disk1 is now genuinely mounted", failed)
	}
}

// TestStorageTargetSync_Startup_ReconcilesAMountpointWhoseOwnUUIDIsMounted
// is the positive half of #404: with s.ArrayStore set (seedTestArray's own
// /mnt/disk1, filesystem UUID uuid-d1), a mountpoint recorded failed still
// clears once the mount table (findmnt, through the real
// disk.ConfirmMountedUUID default — never overridden here) confirms that
// exact filesystem, not merely that something is mounted there.
func TestStorageTargetSync_Startup_ReconcilesAMountpointWhoseOwnUUIDIsMounted(t *testing.T) {
	ctx := context.Background()
	arrayStore, shareStore := newTestArrayAndShareStore(t)
	seedTestArray(t, ctx, arrayStore)

	s := newTestStorageTargetSync(t)
	s.ArrayStore = arrayStore
	s.ShareStore = shareStore
	s.mountFailedMountpoints = map[string]bool{"/mnt/disk1": true}
	s.PoolMounted = func(path string) (bool, error) { return path == "/mnt/disk1", nil }
	runner := s.Runner.(*disk.FakeRunner)
	runner.Script("findmnt", []string{"-n", "-o", "UUID", "/mnt/disk1"}, []byte("uuid-d1\n"), nil)
	disk1 := storageTargetTestMount{where: "/mnt/disk1"}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}}

	if err := s.Startup(ctx, seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	if failed := s.MountFailedMountpoints(); len(failed) != 0 {
		t.Fatalf("MountFailedMountpoints() = %v, want empty — /mnt/disk1 is genuinely mounted with its own filesystem UUID", failed)
	}
}

// TestStorageTargetSync_Startup_DoesNotReconcileAMountpointWithADifferentFilesystemMounted
// is #404's own regression: CodeRabbit's PR 403 nitpick found that
// reconcileMountFailures used to confirm only that something was mounted
// at a slot's own mountpoint, never that it was the slot's own disk — so a
// manual mount or a leftover unit of a different filesystem at the same
// path cleared the recorded failure and GetPool reported the slot active
// even though its own disk had never mounted. With s.ArrayStore set
// (seedTestArray's own /mnt/disk1, filesystem UUID uuid-d1) and the mount
// table (findmnt, through the real disk.ConfirmMountedUUID default)
// reporting a different UUID mounted there, the recorded failure must
// stay in place.
func TestStorageTargetSync_Startup_DoesNotReconcileAMountpointWithADifferentFilesystemMounted(t *testing.T) {
	ctx := context.Background()
	arrayStore, shareStore := newTestArrayAndShareStore(t)
	seedTestArray(t, ctx, arrayStore)

	s := newTestStorageTargetSync(t)
	s.ArrayStore = arrayStore
	s.ShareStore = shareStore
	s.mountFailedMountpoints = map[string]bool{"/mnt/disk1": true}
	s.PoolMounted = func(path string) (bool, error) { return path == "/mnt/disk1", nil }
	runner := s.Runner.(*disk.FakeRunner)
	runner.Script("findmnt", []string{"-n", "-o", "UUID", "/mnt/disk1"}, []byte("uuid-someone-elses-filesystem\n"), nil)
	disk1 := storageTargetTestMount{where: "/mnt/disk1"}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}}

	if err := s.Startup(ctx, seq); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	failed := s.MountFailedMountpoints()
	if !failed["/mnt/disk1"] {
		t.Fatalf("MountFailedMountpoints() = %v, want /mnt/disk1 still recorded — the mounted filesystem is not the slot's own (uuid-d1)", failed)
	}
}

// TestStorageTargetSync_Update_DoesNotReconcileAMountpointWithADifferentFilesystemMounted
// is the same #404 regression on the rebuild path updateTransition runs
// from a live SIGHUP arrival, never only at boot.
func TestStorageTargetSync_Update_DoesNotReconcileAMountpointWithADifferentFilesystemMounted(t *testing.T) {
	ctx := context.Background()
	arrayStore, shareStore := newTestArrayAndShareStore(t)
	seedTestArray(t, ctx, arrayStore)

	s := newTestStorageTargetSync(t)
	s.ArrayStore = arrayStore
	s.ShareStore = shareStore
	s.mountFailedMountpoints = map[string]bool{"/mnt/disk1": true}
	s.PoolMounted = func(path string) (bool, error) { return path == "/mnt/disk1", nil }
	runner := s.Runner.(*disk.FakeRunner)
	runner.Script("findmnt", []string{"-n", "-o", "UUID", "/mnt/disk1"}, []byte("uuid-someone-elses-filesystem\n"), nil)
	disk1 := storageTargetTestMount{where: "/mnt/disk1"}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}}

	s.Update(ctx, seq)

	failed := s.MountFailedMountpoints()
	if !failed["/mnt/disk1"] {
		t.Fatalf("MountFailedMountpoints() = %v, want /mnt/disk1 still recorded — the mounted filesystem is not the slot's own (uuid-d1)", failed)
	}
}

// TestStorageTargetSync_Update_RecordsAFailedDiskMount is finding 4's own
// regression: a same-serial disk that arrives while hoservad is already
// running (the SIGHUP path, never a restart) must still have its own
// mount failure recorded. Before this fix, updateTransition never called
// mountArrayDisks at all — only mountAndConfirmPool (the pool, never the
// physical disk) — so this slot's own mountpoint never reached
// MountFailedMountpoints, and GetPool kept reporting it active even
// though its disk had never actually mounted.
func TestStorageTargetSync_Update_RecordsAFailedDiskMount(t *testing.T) {
	s := newTestStorageTargetSync(t)
	disk1 := storageTargetTestMount{where: "/mnt/disk1", mountErr: errors.New("device dependency never resolved")}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}}

	s.Update(context.Background(), seq)

	failed := s.MountFailedMountpoints()
	if !failed["/mnt/disk1"] {
		t.Fatalf("MountFailedMountpoints() = %v, want /mnt/disk1 recorded — a live SIGHUP arrival must record a mount failure exactly like Startup does", failed)
	}
}

// TestStorageTargetSync_Update_BoundsAMountThatNeverReturns is finding
// 3's own regression, mirroring TestStorageTargetSync_Startup_BoundsA-
// MountThatNeverReturns for the live rebuild path: before this fix,
// updateTransition's own mount call ran on ctx directly, with no bound
// at all — the exact call installReloadHandler's own unconditional first
// rebuild makes, before main.go's own API listeners ever start (a real
// nightly L3 failure this fix closes), so a same-serial disk whose mount
// unit never resolves its device left the API unreachable for however
// long systemd's own default unit-start timeout (90s) took to give up.
func TestStorageTargetSync_Update_BoundsAMountThatNeverReturns(t *testing.T) {
	s := newTestStorageTargetSync(t)
	s.StartupMountTimeout = 50 * time.Millisecond
	disk1 := blockingUntilCancelledMount{where: "/mnt/disk1"}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}}

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- s.UpdateOrError(context.Background(), seq) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Update with a mount that never returns = nil error, want one")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("Update took %v, want it bounded near StartupMountTimeout (%v)", elapsed, s.StartupMountTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Update did not return within 5s of a mount that never returns on its own — it would block installReloadHandler's own synchronous first rebuild, and with it main.go's own API listeners, indefinitely")
	}
	if _, err := os.Stat(s.flagPath()); err == nil {
		t.Fatal("the readiness flag exists even though the mount never confirmed")
	}
}

// signalingBlockingMount is blockingUntilCancelledMount's own cousin, with
// a channel closed the instant Mount actually starts running — so a test
// can wait for a caller to genuinely be inside the mount call (and
// therefore genuinely holding s.mu) before asserting on something else
// that must not be blocked behind it — and a second channel the test
// itself closes only once that assertion has already returned, so Mount
// stays blocked for as long as the test needs rather than resolving on a
// fixed bound a mutation could still race. TestStorageTargetSync_
// MountFailedMountpoints_NeverBlocksBehindALiveMountAttempt found that
// race directly: a 500ms StartupMountTimeout next to a 1s assertion
// window let MountFailedMountpoints go back to sharing s.mu and still
// pass, because Startup's own mount always finished (via that bound)
// before the window ran out either way.
type signalingBlockingMount struct {
	where   string
	started chan struct{}
	release chan struct{}
}

func (m signalingBlockingMount) Where() string { return m.where }
func (m signalingBlockingMount) Mount(ctx context.Context) error {
	close(m.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.release:
		return errors.New("mount released by test")
	}
}
func (m signalingBlockingMount) Unmount(context.Context) error { return nil }

// TestStorageTargetSync_MountFailedMountpoints_NeverBlocksBehindALiveMountAttempt
// is finding 3's own regression: GET /pool (api.Handler's own
// MountFailedSlots hook, wired to MountFailedMountpoints) must never wait
// behind s.mu while Startup or updateTransition holds it across a slow,
// live mount attempt — a nightly L3 failure this fix closes had exactly
// that: GET /pool blocked for the whole span of a stuck `systemctl`
// call, because MountFailedMountpoints used to share s.mu with it.
// StartupMountTimeout is left at its real production default (20s)
// deliberately: the mount below never resolves on its own, only once
// this test's own release channel closes it — well after the assertion
// below has already returned — so nothing here depends on winning a race
// against a fixed bound (see signalingBlockingMount's own doc comment).
func TestStorageTargetSync_MountFailedMountpoints_NeverBlocksBehindALiveMountAttempt(t *testing.T) {
	s := newTestStorageTargetSync(t)
	started := make(chan struct{})
	release := make(chan struct{})
	disk1 := signalingBlockingMount{where: "/mnt/disk1", started: started, release: release}
	seq := &job.ArraySequence{Gate: storageTargetTestGate{ready: true}, Disks: []job.ArrayMount{disk1}}

	startupDone := make(chan struct{})
	go func() {
		_ = s.Startup(context.Background(), seq)
		close(startupDone)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Startup's own mount attempt never started within 5s")
	}

	done := make(chan struct{})
	go func() {
		s.MountFailedMountpoints()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("MountFailedMountpoints() did not return within 1s while Startup's own mount attempt was still in flight, holding s.mu")
	}

	close(release)
	select {
	case <-startupDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Startup did not return within 5s of its own mount being released")
	}
}
