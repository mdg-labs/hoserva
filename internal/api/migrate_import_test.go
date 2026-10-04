package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/store"
)

// importFix is a handler with a migration service over a machine whose parity
// and data disks can be adopted, and a migration_import job that only records
// its params.
type importFix struct {
	h        *api.Handler
	svc      *migrate.Service
	registry *job.Registry
	sessions *store.MigrationSessionStore
	// blockScan, when set, holds every scan job until it is closed.
	blockScan chan struct{}
	params    [][]byte
}

func newImportFix(t *testing.T, more func(*disk.FakeProvider)) *importFix {
	t.Helper()
	h, _, registry := newTestHandler(t)
	disks := disk.NewFakeProvider()
	disks.AddDisk("/dev/sdb", disk.Disk{Serial: "PARITYSERIAL", Size: 2 << 40, Filesystem: "xfs", FSDevice: "/dev/sdb1", FSUUID: "10000000-0000-4000-8000-000000000001", ByIDName: "ata-X_PARITYSERIAL", FSByIDName: "ata-X_PARITYSERIAL-part1"})
	disks.AddDisk("/dev/sdc", disk.Disk{Serial: "DATASERIAL", Size: 1 << 40, Filesystem: "xfs", FSDevice: "/dev/sdc1", FSUUID: "10000000-0000-4000-8000-000000000002", ByIDName: "ata-X_DATASERIAL", FSByIDName: "ata-X_DATASERIAL-part1"})
	if more != nil {
		more(disks)
	}
	f := &importFix{h: h, registry: registry, sessions: store.NewMigrationSessionStore(openTestDB(t))}
	f.svc = &migrate.Service{
		Dir:      filepath.Join(t.TempDir(), "migrate"),
		Scanner:  &migrate.Scanner{Disks: disks, UIDOwner: func(int) (string, error) { return "", nil }},
		Sessions: f.sessions,
		JobEnded: func(ctx context.Context, id string) (string, bool, error) {
			j, err := h.Store.Get(ctx, id)
			if err != nil {
				return "", false, err
			}
			return string(j.Status), j.Status.Terminal(), nil
		},
	}
	h.Migration = f.svc
	h.ArrayStore = store.NewArrayStore(openTestDB(t))
	h.Scheduler.SetMigrationPending(h.ArrayStore.MigrationPending)
	f.svc.Pending = h.ArrayStore.MigrationPending
	scan := job.RunMigrationScan(f.svc.RunScan)
	registry.Register(job.TypeMigrationScan, false, func(ctx context.Context, rc *job.RunContext) error {
		if f.blockScan != nil {
			select {
			case <-f.blockScan:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return scan(ctx, rc)
	})
	registry.Register(job.TypeMigrationImport, false, func(_ context.Context, rc *job.RunContext) error {
		f.params = append(f.params, rc.Params())
		return nil
	})
	return f
}

func importReq(confirm bool, roles ...apiv1.MigrationImportDisk) *apiv1.MigrationImportRequest {
	return &apiv1.MigrationImportRequest{Roles: roles, Confirm: confirm}
}

func importRole(role apiv1.MigrationImportRole, serial string) apiv1.MigrationImportDisk {
	return apiv1.MigrationImportDisk{Role: role, Serial: apiv1.NewOptString(serial)}
}

// scanned runs the scan of flashZip on h and waits for it.
func scanned(t *testing.T, f *importFix) {
	t.Helper()
	h, svc := f.h, f.svc
	ctx := context.Background()
	j, err := h.StartMigrationScan(ctx, scanRequest(flashZip(t, "7.3.2", nil), false))
	if err != nil {
		t.Fatalf("StartMigrationScan: %v", err)
	}
	awaitJob(t, h.Scheduler, j.ID.String())
	if st, err := svc.State(ctx); err != nil || st.Report == nil {
		t.Fatalf("State after the scan = %+v, %v", st, err)
	}
}

func TestHandler_StartMigrationImport_Return501WithoutTheMigratorOrTheArrayStore(t *testing.T) {
	h, _, _ := newTestHandler(t)
	_, err := h.StartMigrationImport(context.Background(), importReq(true, importRole(apiv1.MigrationImportRoleData, "x")))
	if st, code := statusOf(h, err); st != 501 || code != "not_configured" {
		t.Errorf("StartMigrationImport = %d %s, want 501 not_configured", st, code)
	}
}

func TestHandler_StartMigrationImport_QueuesTheResolvedPlanAndNothingBeforeTheChecks(t *testing.T) {
	ctx := context.Background()
	f := newImportFix(t, nil)
	h := f.h
	good := importReq(true, importRole(apiv1.MigrationImportRoleParity, "PARITYSERIAL"), importRole(apiv1.MigrationImportRoleData, "DATASERIAL"))

	// No report yet.
	if _, err := h.StartMigrationImport(ctx, good); err == nil {
		t.Fatal("an import before any scan was accepted")
	} else if st, code := statusOf(h, err); st != 404 || code != "no_migration_report" {
		t.Errorf("before a scan = %d %s, want 404 no_migration_report", st, code)
	}

	scanned(t, f)

	// Nothing is queued without the user's confirmation, whatever the mapping.
	if _, err := h.StartMigrationImport(ctx, importReq(false, good.Roles...)); err == nil {
		t.Fatal("an import without confirm was accepted")
	} else if st, code := statusOf(h, err); st != 409 || code != "confirmation_required" {
		t.Errorf("without confirm = %d %s, want 409 confirmation_required", st, code)
	}
	if jobs, _ := h.Store.List(ctx, job.ListFilter{}); countType(jobs, job.TypeMigrationImport) != 0 {
		t.Fatalf("a refused request queued an import job")
	}

	j, err := h.StartMigrationImport(ctx, good)
	if err != nil {
		t.Fatalf("StartMigrationImport: %v", err)
	}
	if j.Type != apiv1.JobTypeMigrationImport || j.Class != apiv1.JobClassTopology {
		t.Errorf("job = %+v, want a topology migration_import", j)
	}
	if done := awaitJob(t, h.Scheduler, j.ID.String()); done.Status != job.StatusSucceeded {
		t.Fatalf("job ended %s", done.Status)
	}
	if len(f.params) != 1 {
		t.Fatalf("the job ran %d times", len(f.params))
	}
	var got job.MigrationImportParams
	if err := json.Unmarshal(f.params[0], &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Plan.Data) != 1 || got.Plan.Data[0].Serial != "DATASERIAL" || got.Plan.Data[0].MountSource != "/dev/disk/by-id/ata-X_DATASERIAL-part1" || len(got.Plan.Parity) != 1 {
		t.Errorf("params plan = %+v, want the resolved plan with the data disk bound to its own by-id link", got.Plan)
	}
	if len(got.Assignments) != 2 {
		t.Errorf("params assignments = %+v", got.Assignments)
	}
}

func countType(jobs []*job.Job, typ job.Type) int {
	n := 0
	for _, j := range jobs {
		if j.Type == typ {
			n++
		}
	}
	return n
}

func TestHandler_StartMigrationImport_RefusesAMappingThatBreaksARule(t *testing.T) {
	ctx := context.Background()
	f := newImportFix(t, func(p *disk.FakeProvider) {
		p.AddDisk("/dev/sdq", disk.Disk{Serial: "STICKSERIAL", Size: 16 * disk.GB, Filesystem: "vfat", Label: "UNRAID", FSUUID: "ABCD-1234", FSDevice: "/dev/sdq1"})
		p.AddDisk("/dev/sdr", disk.Disk{Serial: "BOOTSERIAL", Size: 4 * disk.TB, Boot: true, Filesystem: "ext4", FSUUID: "10000000-0000-4000-8000-000000000009", FSDevice: "/dev/sdr1"})
	})
	h := f.h
	scanned(t, f)
	for _, tc := range []struct {
		name   string
		roles  []apiv1.MigrationImportDisk
		status int
		code   string
	}{
		{"the stick as data", []apiv1.MigrationImportDisk{importRole(apiv1.MigrationImportRoleParity, "PARITYSERIAL"), importRole(apiv1.MigrationImportRoleData, "STICKSERIAL")}, 409, "unraid_stick"},
		{"the stick as parity", []apiv1.MigrationImportDisk{importRole(apiv1.MigrationImportRoleParity, "STICKSERIAL"), importRole(apiv1.MigrationImportRoleData, "DATASERIAL")}, 409, "unraid_stick"},
		{"the stick as cache", []apiv1.MigrationImportDisk{importRole(apiv1.MigrationImportRoleParity, "PARITYSERIAL"), importRole(apiv1.MigrationImportRoleData, "DATASERIAL"), importRole(apiv1.MigrationImportRoleCache, "STICKSERIAL")}, 409, "unraid_stick"},
		{"the boot disk as parity", []apiv1.MigrationImportDisk{importRole(apiv1.MigrationImportRoleParity, "BOOTSERIAL"), importRole(apiv1.MigrationImportRoleData, "DATASERIAL")}, 400, "invalid_import_roles"},
		{"the boot disk as data", []apiv1.MigrationImportDisk{importRole(apiv1.MigrationImportRoleParity, "PARITYSERIAL"), importRole(apiv1.MigrationImportRoleData, "BOOTSERIAL")}, 400, "invalid_import_roles"},
		{"a disk that is not there", []apiv1.MigrationImportDisk{importRole(apiv1.MigrationImportRoleParity, "PARITYSERIAL"), importRole(apiv1.MigrationImportRoleData, "NOPE")}, 400, "invalid_import_roles"},
		{"Unraid's parity disk as data", []apiv1.MigrationImportDisk{importRole(apiv1.MigrationImportRoleParity, "DATASERIAL"), importRole(apiv1.MigrationImportRoleData, "PARITYSERIAL")}, 400, "invalid_import_roles"},
		{"no parity", []apiv1.MigrationImportDisk{importRole(apiv1.MigrationImportRoleData, "DATASERIAL")}, 400, "invalid_import_roles"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.StartMigrationImport(ctx, importReq(true, tc.roles...))
			if err == nil {
				t.Fatal("accepted")
			}
			if st, code := statusOf(h, err); st != tc.status || code != tc.code {
				t.Errorf("= %d %s (%v), want %d %s", st, code, err, tc.status, tc.code)
			}
			if jobs, _ := h.Store.List(ctx, job.ListFilter{}); countType(jobs, job.TypeMigrationImport) != 0 {
				t.Errorf("a refused request queued an import job")
			}
		})
	}
}

func TestHandler_StartMigrationImport_RefusesAnAbandonedScanAndAForeignArrayButNotItsOwnRetry(t *testing.T) {
	ctx := context.Background()
	good := importReq(true, importRole(apiv1.MigrationImportRoleParity, "PARITYSERIAL"), importRole(apiv1.MigrationImportRoleData, "DATASERIAL"))
	arrays := func(f *importFix, pending bool) {
		put := f.h.ArrayStore.PutArray
		var err error
		if pending {
			err = f.h.ArrayStore.PutPendingArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []store.ArrayDisk{
				{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "u", Mountpoint: "/mnt/disk1"},
			}, []store.RecordedDisk{{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sdb", Size: 1}})
		} else {
			err = put(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []store.ArrayDisk{
				{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdz", Filesystem: "xfs", FSUUID: "u", Mountpoint: "/mnt/disk1"},
			})
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Run("an array that is not a pending import's", func(t *testing.T) {
		f := newImportFix(t, nil)
		scanned(t, f)
		arrays(f, false)
		_, err := f.h.StartMigrationImport(ctx, good)
		if s, code := statusOf(f.h, err); s != 409 || code != "array_exists" {
			t.Errorf("StartMigrationImport over an ordinary array = %d %s, want 409 array_exists", s, code)
		}
		if len(f.params) != 0 {
			t.Error("the import ran")
		}
	})
	t.Run("a pending import's own retry", func(t *testing.T) {
		f := newImportFix(t, nil)
		scanned(t, f)
		arrays(f, true)
		j, err := f.h.StartMigrationImport(ctx, good)
		if err != nil {
			t.Fatalf("a retry of a pending import = %v, want it queued", err)
		}
		awaitJob(t, f.h.Scheduler, j.ID.String())
	})
	t.Run("a scan that has not finished", func(t *testing.T) {
		f := newImportFix(t, nil)
		scanned(t, f)
		f.blockScan = make(chan struct{})
		defer close(f.blockScan)
		if _, err := f.h.StartMigrationScan(ctx, scanRequest(flashZip(t, "7.3.2", nil), false)); err != nil {
			t.Fatal(err)
		}
		_, err := f.h.StartMigrationImport(ctx, good)
		if s, code := statusOf(f.h, err); s != 409 || code != "scan_not_finished" {
			t.Errorf("StartMigrationImport during a scan = %d %s, want 409 scan_not_finished", s, code)
		}
	})
	t.Run("a no-go scan", func(t *testing.T) {
		f := newImportFix(t, func(p *disk.FakeProvider) {
			// The parity slot is filled by this machine's boot disk: refused.
			p.AddDisk("/dev/sdb", disk.Disk{Serial: "PARITYSERIAL", Size: 2 << 40, Boot: true})
		})
		scanned(t, f)
		if st, _ := f.svc.State(ctx); st.Report.Verdict != migrate.VerdictNoGo {
			t.Fatalf("verdict = %s, want no_go: %+v", st.Report.Verdict, st.Report.Rows)
		}
		_, err := f.h.StartMigrationImport(ctx, good)
		if s, code := statusOf(f.h, err); s != 409 || code != "migration_no_go" {
			t.Errorf("StartMigrationImport = %d %s, want 409 migration_no_go", s, code)
		}
	})
}

// While an import is pending the scheduler refuses every parity, array-write
// and topology job, a scan and a forget with the one code.
func TestHandler_WhileAnImportIsPendingStorageJobsAndTheSessionsDeletionAreRefused(t *testing.T) {
	ctx := context.Background()
	f := newImportFix(t, nil)
	h := f.h
	scanned(t, f)
	if err := h.ArrayStore.PutPendingArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "u", Mountpoint: "/mnt/disk1"},
	}, []store.RecordedDisk{{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sdb", Size: 1}}); err != nil {
		t.Fatal(err)
	}
	if st, err := h.GetMigration(ctx); err != nil || st.Phase != apiv1.MigrationPhaseImported {
		t.Fatalf("GetMigration = %+v, %v, want the imported phase", st, err)
	}
	f.registry.Register(job.TypeSync, false, func(context.Context, *job.RunContext) error { return nil })
	for name, call := range map[string]func() error{
		"a scan": func() error {
			_, err := h.StartMigrationScan(ctx, scanRequest(flashZip(t, "7.3.2", nil), false))
			return err
		},
		"a forget": func() error { return h.ForgetMigration(ctx) },
		"a sync":   func() error { _, err := h.StartSync(ctx, &apiv1.StartSyncRequest{}); return err },
	} {
		err := call()
		if err == nil {
			t.Errorf("%s was accepted while an import is pending", name)
			continue
		}
		if st, code := statusOf(h, err); st != 409 || code != "migration_in_progress" {
			t.Errorf("%s = %d %s (%v), want 409 migration_in_progress", name, st, code, err)
		}
	}
	if st, err := h.GetMigration(ctx); err != nil || !st.Report.Set {
		t.Errorf("the refused forget removed the report: %+v, %v", st, err)
	}
	if !errors.Is(f.svc.Forget(ctx), migrate.ErrImportPending) {
		t.Error("the service allowed a forget while an import is pending")
	}
}

// The array setup refuses the Unraid stick in every role before anything is
// queued or formatted.
func TestHandler_CreateArray_RefusesTheUnraidStick(t *testing.T) {
	ctx := context.Background()
	for _, role := range []apiv1.ArrayDiskRole{apiv1.ArrayDiskRoleData, apiv1.ArrayDiskRoleParity, apiv1.ArrayDiskRoleCache} {
		h, _, p := newArrayHandler(t)
		p.AddDisk("/dev/sdq", disk.Disk{Size: 16 * disk.GB, Filesystem: "vfat", Label: "UNRAID", FSUUID: "ABCD-1234", FSDevice: "/dev/sdq1"})
		disks := []apiv1.ArrayDiskAssignment{xfsAssignment("/dev/sda", apiv1.ArrayDiskRoleParity), xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleData)}
		stick := xfsAssignment("/dev/sdq", role)
		if role == apiv1.ArrayDiskRoleParity {
			disks = []apiv1.ArrayDiskAssignment{stick, xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleData)}
		} else {
			disks = append(disks, stick)
		}
		_, err := h.CreateArray(ctx, createArrayReq("ERASE whatever", disks...))
		if st, code := statusOf(h, err); st != 409 || code != "unraid_stick" {
			t.Errorf("%s: CreateArray = %d %s (%v), want 409 unraid_stick before the confirmation is even read", role, st, code, err)
		}
		if calls := p.FormatCalls(); len(calls) != 0 {
			t.Errorf("%s: formatted %v", role, calls)
		}
	}
}
