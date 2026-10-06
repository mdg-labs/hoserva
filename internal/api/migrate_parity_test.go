package api_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// freeSpace answers every data disk's free space with one figure, as statfs
// would for the adopted disks' mounts.
type freeSpace int64

func (f freeSpace) StatSpace(context.Context, string) (pool.SpaceStat, error) {
	return pool.SpaceStat{TotalBytes: 4 * disk.TB, FreeBytes: int64(f)}, nil
}

// parityFix is an import fixture whose array is adopted and pending its point of
// no return, with the migration_parity job recording its params.
type parityFix struct {
	*importFix
	queued [][]byte
}

func newParityFix(t *testing.T) *parityFix {
	t.Helper()
	f := &parityFix{importFix: newImportFix(t, nil)}
	f.svc.Record = func(ctx context.Context) ([]store.ArrayDisk, []store.RecordedDisk, error) {
		_, disks, err := f.h.ArrayStore.GetArray(ctx)
		if err != nil {
			return nil, nil, err
		}
		rec, err := f.h.ArrayStore.RecordedDisks(ctx)
		return disks, rec, err
	}
	f.svc.Finishing = f.h.ArrayStore.MigrationFinishing
	f.svc.Space = freeSpace(500 * disk.GB)
	f.h.Scheduler.SetMigrationPending(f.h.ArrayStore.MigrationUnfinished)
	f.registry.Register(job.TypeMigrationParity, false, func(_ context.Context, rc *job.RunContext) error {
		f.queued = append(f.queued, rc.Params())
		return nil
	})
	scanned(t, f.importFix)
	return f
}

// adopt records the adopted array the import leaves: the data disk, and the
// parity disk recorded and left unformatted.
func (f *parityFix) adopt(t *testing.T) {
	t.Helper()
	if err := f.h.ArrayStore.PutPendingArray(context.Background(), store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "10000000-0000-4000-8000-000000000002", Serial: "DATASERIAL", ByIDName: "ata-X_DATASERIAL", Mountpoint: "/mnt/disk1"},
	}, []store.RecordedDisk{{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sdb", Size: 2 << 40, Serial: "PARITYSERIAL", ByIDName: "ata-X_PARITYSERIAL"}}); err != nil {
		t.Fatal(err)
	}
}

func (f *parityFix) setVerify(t *testing.T, status string) {
	t.Helper()
	ctx := context.Background()
	row, _, err := f.sessions.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status == "" {
		row.Verify = nil
	} else if row.Verify, err = json.Marshal(migrate.VerifyResult{Status: status, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := f.sessions.Put(ctx, row); err != nil {
		t.Fatal(err)
	}
}

func (f *parityFix) assertNothingQueued(t *testing.T, when string) {
	t.Helper()
	if len(f.queued) != 0 {
		t.Errorf("%s: a migration_parity job was queued: %s", when, f.queued)
	}
	jobs, err := f.h.Store.List(context.Background(), job.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Type == job.TypeMigrationParity {
			t.Errorf("%s: a migration_parity job was persisted: %+v", when, j)
		}
	}
}

func TestHandler_InitializeMigrationParity_Return501WithoutTheMigrator(t *testing.T) {
	h, _, _ := newTestHandler(t)
	_, err := h.InitializeMigrationParity(context.Background(), &apiv1.MigrationInitializeParityRequest{Confirmation: "x"})
	if st, code := statusOf(h, err); st != 501 || code != "not_configured" {
		t.Errorf("InitializeMigrationParity = %d %s, want 501 not_configured", st, code)
	}
}

// The API refuses parity initialisation without a completed, passing verify, with
// a distinct error and before the confirmation is even looked at, and queues
// nothing.
func TestHandler_InitializeMigrationParity_IsRefusedWithoutAPassingVerify(t *testing.T) {
	ctx := context.Background()
	f := newParityFix(t)
	req := &apiv1.MigrationInitializeParityRequest{Confirmation: "ERASE /dev/sdb"}

	_, err := f.h.InitializeMigrationParity(ctx, req)
	if st, code := statusOf(f.h, err); st != 409 || code != "no_import_pending" {
		t.Errorf("before an import = %d %s, want 409 no_import_pending", st, code)
	}
	f.assertNothingQueued(t, "before an import")

	f.adopt(t)
	for name, status := range map[string]string{"no verify has run": "", "a verify is running": migrate.VerifyRunning, "the latest verify failed": migrate.VerifyFailed} {
		f.setVerify(t, status)
		for _, confirmation := range []string{"", req.Confirmation} {
			_, err := f.h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: confirmation})
			if st, code := statusOf(f.h, err); st != 409 || code != "verify_required" {
				t.Errorf("%s (confirmation %q) = %d %s, want 409 verify_required", name, confirmation, st, code)
			}
		}
		if m, err := f.h.GetMigration(ctx); err != nil || m.ParityInit.IsSet() {
			t.Errorf("%s: getMigration offers parityInit = %+v, %v", name, m.ParityInit, err)
		}
	}
	f.assertNothingQueued(t, "without a passing verify")
}

// A pool whose every data disk is below its minfreespace could not create the
// share directories once the disks are writable, after the parity disk and the
// cache were erased: the API refuses it as a conflict with the reason, offers no
// confirmation and queues nothing, whatever the request carries.
func TestHandler_InitializeMigrationParity_IsRefusedWhenNoDataDiskHasThePoolsMinFreeSpace(t *testing.T) {
	ctx := context.Background()
	f := newParityFix(t)
	f.adopt(t)
	f.setVerify(t, migrate.VerifyPassed)
	f.svc.Space = freeSpace(20 * disk.GB)

	for _, confirmation := range []string{"", "ERASE /dev/sdb"} {
		_, err := f.h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: confirmation})
		if st, code := statusOf(f.h, err); st != 409 || code != "pool_below_min_free_space" {
			t.Errorf("confirmation %q = %d %s, want 409 pool_below_min_free_space", confirmation, st, code)
		}
	}
	m, err := f.h.GetMigration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pi, ok := m.ParityInit.Get(); !ok || pi.Confirmation.IsSet() || !pi.Problem.IsSet() || len(pi.Erases) != 0 {
		t.Errorf("parityInit = %+v, want a problem and no confirmation", pi)
	}
	f.assertNothingQueued(t, "a pool below its minimum free space")
}

func TestHandler_InitializeMigrationParity_NeedsTheExactTypedConfirmationGetMigrationGives(t *testing.T) {
	ctx := context.Background()
	f := newParityFix(t)
	f.adopt(t)
	f.setVerify(t, migrate.VerifyPassed)

	m, err := f.h.GetMigration(ctx)
	if err != nil || m.Phase != apiv1.MigrationPhaseVerified {
		t.Fatalf("phase = %s, %v, want verified", m.Phase, err)
	}
	pi, ok := m.ParityInit.Get()
	if !ok || pi.Finishing || pi.Problem.IsSet() {
		t.Fatalf("parityInit = %+v", pi)
	}
	want := pi.Confirmation.Or("")
	if want != "ERASE /dev/sdb" || len(pi.Erases) != 1 || pi.Erases[0].Role != apiv1.MigrationParityEraseRoleParity || pi.Erases[0].Device != "/dev/sdb" || pi.Erases[0].Partition {
		t.Errorf("confirmation = %q, erases = %+v, want the parity disk only: the data disk is never erased", want, pi.Erases)
	}
	if pi.UnprotectedWindow == "" || len(pi.Rollback) == 0 {
		t.Errorf("parityInit lacks the unprotected window or the rollback: %+v", pi)
	}

	for name, got := range map[string]string{
		"missing":                  "",
		"a data disk added":        "ERASE /dev/sdb, /dev/sdc",
		"another device":           "ERASE /dev/sdc",
		"lower case":               "erase /dev/sdb",
		"the finishing phrase":     disk.ParityInitFinishConfirmation,
		"the adoption-only phrase": "ADOPT ONLY — NOTHING ERASED",
	} {
		_, err := f.h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: got})
		if st, code := statusOf(f.h, err); st != 409 || code != "confirmation_required" {
			t.Errorf("%s = %d %s, want 409 confirmation_required", name, st, code)
		}
	}
	if _, err := f.h.InitializeMigrationParity(ctx, nil); err == nil {
		t.Error("a request with no body was accepted")
	}
	f.assertNothingQueued(t, "a wrong confirmation")

	j, err := f.h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: want})
	if err != nil {
		t.Fatalf("InitializeMigrationParity: %v", err)
	}
	if j.Type != apiv1.JobTypeMigrationParity || j.Class != apiv1.JobClassTopology {
		t.Errorf("job = %+v", j)
	}
	awaitJob(t, f.h.Scheduler, j.ID.String())
	if len(f.queued) != 1 {
		t.Fatalf("queued = %v", f.queued)
	}
	var p job.MigrationParityParams
	if err := json.Unmarshal(f.queued[0], &p); err != nil || p.Confirmation != want {
		t.Errorf("params = %s, %v, want the confirmation typed", f.queued[0], err)
	}
}

// A disk that went away, or may not be erased, is a refusal of the request with
// the error of the mapping, and no confirmation is offered for it.
func TestHandler_InitializeMigrationParity_ARecordedDiskThatChangedIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newParityFix(t)
	f.adopt(t)
	f.setVerify(t, migrate.VerifyPassed)
	// The parity disk is no longer on this machine.
	f.svc.Scanner.Disks = disk.NewFakeProvider()
	_, err := f.h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: "ERASE /dev/sdb"})
	if st, code := statusOf(f.h, err); st != 400 || code != "invalid_import_roles" {
		t.Errorf("a parity disk that is gone = %d %s, want 400 invalid_import_roles", st, code)
	}
	m, err := f.h.GetMigration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pi, ok := m.ParityInit.Get(); !ok || pi.Confirmation.IsSet() || !pi.Problem.IsSet() || len(pi.Erases) != 0 {
		t.Errorf("parityInit = %+v, want a problem and no confirmation", pi)
	}
	f.assertNothingQueued(t, "a disk that changed")
}

// An initialisation that stopped after the disks were formatted is finished by
// running it again with the finishing confirmation, which erases nothing.
func TestHandler_InitializeMigrationParity_AnUnfinishedInitialisationIsFinishedWithItsOwnConfirmation(t *testing.T) {
	ctx := context.Background()
	f := newParityFix(t)
	f.adopt(t)
	if err := f.h.ArrayStore.RecordParityInit(ctx, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Serial: "PARITYSERIAL", Mountpoint: "/mnt/parity1"},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := f.h.GetMigration(ctx)
	if err != nil || m.Phase != apiv1.MigrationPhaseInitializing {
		t.Fatalf("phase = %s, %v, want initializing", m.Phase, err)
	}
	pi, _ := m.ParityInit.Get()
	if !pi.Finishing || pi.Confirmation.Or("") != disk.ParityInitFinishConfirmation || len(pi.Erases) != 0 {
		t.Errorf("parityInit = %+v", pi)
	}
	if _, err := f.h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: "ERASE /dev/sdb"}); err == nil {
		t.Error("the erase confirmation finished an initialisation that has nothing left to erase")
	}
	f.assertNothingQueued(t, "a wrong confirmation")
	j, err := f.h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: disk.ParityInitFinishConfirmation})
	if err != nil {
		t.Fatalf("InitializeMigrationParity: %v", err)
	}
	awaitJob(t, f.h.Scheduler, j.ID.String())
}
