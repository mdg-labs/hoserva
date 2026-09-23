package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newRegistryTestScheduler builds a job.Scheduler backed by a real,
// migrated SQLite database — the same construction run's own registration
// block uses (job.NewStore, job.NewLogStore, job.NewHub, job.NewRegistry)
// — so a test here proves a registry.Register call of the exact shape
// main.go uses actually runs its RunFunc through a real Scheduler.Submit,
// not just that RunFix itself works against a fake (already covered by
// internal/job/params_test.go).
func newRegistryTestScheduler(t *testing.T, registry *job.Registry) *job.Scheduler {
	t.Helper()
	s, _ := newRegistryTestSchedulerAndDB(t, registry)
	return s
}

// newRegistryTestSchedulerAndDB is newRegistryTestScheduler, also
// returning the underlying database — TestRegistry_TypeDiskAddRunsRunDiskAdd
// and TestRegistry_TypeDiskReplaceRunsRunDiskReplace need it to build a
// store.ArrayStore sharing the scheduler's own jobs table, exactly as
// main.go's single db serves both.
func newRegistryTestSchedulerAndDB(t *testing.T, registry *job.Registry) (*job.Scheduler, *sql.DB) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-registry-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	return job.NewScheduler(job.NewStore(db), job.NewLogStore(t.TempDir()), job.NewHub(), registry), db
}

// TestRegistry_TypeFixRunsRunFix proves job.TypeFix registered the same
// way main.go registers it — registry.Register(job.TypeFix, false,
// job.RunFix(parityEngine)) — actually reaches parity.Engine.Fix through a
// real Scheduler.Submit, rather than failing with
// job.ErrJobTypeNotRegistered the way an unregistered daemon does today
// (#244). TypeSync/TypeScrub, registered the same way in the same block,
// have no equivalent startup-level test in this package.
func TestRegistry_TypeFixRunsRunFix(t *testing.T) {
	ctx := context.Background()
	eng := parity.NewFakeEngine()
	wantErr := errors.New("fix: scripted failure")
	eng.ScriptFix(nil, wantErr)

	registry := job.NewRegistry()
	registry.Register(job.TypeFix, false, job.RunFix(eng))
	s := newRegistryTestScheduler(t, registry)

	params, err := json.Marshal(job.FixParams{Confirm: true})
	if err != nil {
		t.Fatalf("marshaling FixParams: %v", err)
	}
	j, err := s.Submit(ctx, job.TypeFix, nil, params)
	if err != nil {
		t.Fatalf("Submit(TypeFix): %v — TypeFix must be registered like TypeSync/TypeScrub", err)
	}

	finished, err := s.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusFailed {
		t.Fatalf("status = %s, want failed (scripted Fix error)", finished.Status)
	}
	if finished.ErrorMessage != wantErr.Error() {
		t.Fatalf("error = %q, want %q — job.RunFix(eng) must have reached eng.Fix", finished.ErrorMessage, wantErr.Error())
	}
}

// TestRegistry_TypeDiskAddRunsRunDiskAdd proves job.TypeDiskAdd registered
// the same way main.go registers it — registry.Register(job.TypeDiskAdd,
// false, job.RunDiskAdd(job.DiskAddDeps{...})) — actually reaches
// disk.FormatForAddition and persists the new disk through a real
// Scheduler.Submit, rather than failing with job.ErrJobTypeNotRegistered
// the way an unregistered daemon does today (#288).
func TestRegistry_TypeDiskAddRunsRunDiskAdd(t *testing.T) {
	ctx := context.Background()
	registry := job.NewRegistry()
	s, db := newRegistryTestSchedulerAndDB(t, registry)
	arrayStore := store.NewArrayStore(db)

	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Now(),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	r := disk.NewFakeRunner()
	r.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdc"}, []byte("uuid-added\n"), nil)

	registry.Register(job.TypeDiskAdd, false, job.RunDiskAdd(job.DiskAddDeps{
		Provider:  p,
		Runner:    r,
		Store:     arrayStore,
		Generator: cfggen.NewGenerator(t.TempDir()),
		Mounter:   disk.NewFakeMounter(),
	}))

	newDisk := disk.AssignedDisk{Device: "/dev/sdc", Filesystem: disk.XFS}
	params, err := json.Marshal(job.DiskAddParams{
		Confirmation: job.SingleDiskConfirmation(newDisk),
		Disk:         newDisk,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB, "/dev/sdc": 4 * disk.TB},
	})
	if err != nil {
		t.Fatalf("marshaling DiskAddParams: %v", err)
	}
	j, err := s.Submit(ctx, job.TypeDiskAdd, nil, params)
	if err != nil {
		t.Fatalf("Submit(TypeDiskAdd): %v — TypeDiskAdd must be registered like TypeDiskFormat", err)
	}
	finished, err := s.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded — job.RunDiskAdd must have reached disk.FormatForAddition and store.AddDataDisk", finished.Status, finished.ErrorMessage)
	}
	added, err := arrayStore.GetDataDiskByMountpoint(ctx, "/mnt/disk2")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint(/mnt/disk2): %v", err)
	}
	if added.Device != "/dev/sdc" {
		t.Fatalf("added disk device = %q, want /dev/sdc", added.Device)
	}
}

// TestRegistry_TypeDiskReplaceRunsRunDiskReplace is
// TestRegistry_TypeDiskAddRunsRunDiskAdd for job.TypeDiskReplace: it
// proves main.go's own registration shape reaches disk.FormatForAddition,
// store.ReplaceDataDisk and parity.Engine.Fix through a real
// Scheduler.Submit (#288).
func TestRegistry_TypeDiskReplaceRunsRunDiskReplace(t *testing.T) {
	ctx := context.Background()
	registry := job.NewRegistry()
	s, db := newRegistryTestSchedulerAndDB(t, registry)
	arrayStore := store.NewArrayStore(db)

	// A cache disk is what satisfies Q18's parity-count+2 (=3) content-file
	// copy requirement with only one parity and one data disk — the same
	// reason disk_lifecycle_lab_test.go and disk_lifecycle_handler_test.go
	// both seed one.
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Now(),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdcache", Filesystem: "xfs", FSUUID: "uuid-c", Mountpoint: "/mnt/cache"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdcache", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdz", disk.Disk{Size: 4 * disk.TB})
	r := disk.NewFakeRunner()
	r.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdz"}, []byte("uuid-new\n"), nil)
	r.Script("findmnt", []string{"-n", "-o", "UUID", "/mnt/disk1"}, []byte("uuid-new\n"), nil)

	eng := parity.NewFakeEngine()
	eng.SetStatus(parity.ParityStatus{DataMounts: map[string]string{"d1": "/mnt/disk1"}})
	eng.ScriptFix([]parity.Progress{{}}, nil)

	registry.Register(job.TypeDiskReplace, true, job.RunDiskReplace(job.DiskReplaceDeps{
		Provider:  p,
		Runner:    r,
		Store:     arrayStore,
		Generator: cfggen.NewGenerator(t.TempDir()),
		Mounter:   disk.NewFakeMounter(),
		Parity:    eng,
	}))

	replacement := disk.AssignedDisk{Device: "/dev/sdz", Filesystem: disk.XFS}
	params, err := json.Marshal(job.DiskReplaceParams{
		Confirmation: job.SingleDiskConfirmation(replacement),
		Mountpoint:   "/mnt/disk1",
		Disk:         replacement,
		Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdz": 4 * disk.TB},
	})
	if err != nil {
		t.Fatalf("marshaling DiskReplaceParams: %v", err)
	}
	j, err := s.Submit(ctx, job.TypeDiskReplace, nil, params)
	if err != nil {
		t.Fatalf("Submit(TypeDiskReplace): %v — TypeDiskReplace must be registered like TypeDiskFormat", err)
	}
	finished, err := s.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded — job.RunDiskReplace must have reached disk.FormatForAddition, store.ReplaceDataDisk and eng.Fix", finished.Status, finished.ErrorMessage)
	}
	switched, err := arrayStore.GetDataDiskByMountpoint(ctx, "/mnt/disk1")
	if err != nil {
		t.Fatalf("GetDataDiskByMountpoint(/mnt/disk1): %v", err)
	}
	if switched.Device != "/dev/sdz" {
		t.Fatalf("disk1 device = %q, want /dev/sdz", switched.Device)
	}
}
