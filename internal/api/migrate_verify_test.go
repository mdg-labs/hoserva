package api_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/store"
)

func TestHandler_StartMigrationVerify_Return501WithoutTheMigrator(t *testing.T) {
	h, _, _ := newTestHandler(t)
	_, err := h.StartMigrationVerify(context.Background())
	if st, code := statusOf(h, err); st != 501 || code != "not_configured" {
		t.Errorf("StartMigrationVerify = %d %s, want 501 not_configured", st, code)
	}
}

// A verify is refused before anything is queued unless an import is pending its
// point of no return and the session holds a baseline.
func TestHandler_StartMigrationVerify_IsRefusedWithoutAPendingImportOrABaseline(t *testing.T) {
	ctx := context.Background()
	f := newImportFix(t, nil)
	h := f.h
	scanned(t, f)

	_, err := h.StartMigrationVerify(ctx)
	if st, code := statusOf(h, err); st != 409 || code != "no_import_pending" {
		t.Errorf("before an import = %d %s, want 409 no_import_pending", st, code)
	}

	if err := h.ArrayStore.PutPendingArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "u", Mountpoint: "/mnt/disk1"},
	}, []store.RecordedDisk{{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sdb", Size: 1}}); err != nil {
		t.Fatal(err)
	}
	_, err = h.StartMigrationVerify(ctx)
	if st, code := statusOf(h, err); st != 409 || code != "no_migration_baseline" {
		t.Errorf("with an import pending and no baseline = %d %s, want 409 no_migration_baseline", st, code)
	}
	if jobs, err := h.Store.List(ctx, job.ListFilter{}); err != nil {
		t.Fatal(err)
	} else {
		for _, j := range jobs {
			if string(j.Type) == "migration_verify" {
				t.Errorf("a refused verify queued %+v", j)
			}
		}
	}
}

// getMigration serves the verify result and moves the phase with it, only while
// an import is pending.
func TestHandler_GetMigration_ServesTheVerifyResultAndItsPhase(t *testing.T) {
	ctx := context.Background()
	f := newImportFix(t, nil)
	h := f.h
	scanned(t, f)

	bad := migrate.VerifyScope{
		Name: "disk1", Expected: migrate.VerifyCounts{Files: 3, Bytes: 30}, Found: migrate.VerifyCounts{Files: 3, Bytes: 25},
		Hashed: 3, SizeChanged: migrate.VerifyList{Total: 1, Paths: []string{"media/a.txt"}},
	}
	result := migrate.VerifyResult{
		Status: migrate.VerifyFailed, StartedAt: time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC),
		Disks: []migrate.VerifyScope{bad}, Shares: []migrate.VerifyScope{{Name: "media"}},
		Duplicates: 1, DuplicateSample: []migrate.DuplicatePath{{Path: "media/b.txt", Disks: []string{"disk1", "disk2"}}},
	}
	put := func(r migrate.VerifyResult) {
		row, _, err := f.sessions.Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if row.Verify, err = json.Marshal(r); err != nil {
			t.Fatal(err)
		}
		if err := f.sessions.Put(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	put(result)

	if m, err := h.GetMigration(ctx); err != nil || m.Phase != apiv1.MigrationPhaseScanned || m.Verify.IsSet() {
		t.Fatalf("with no import pending = %+v, %v, want the scanned phase and no verify", m, err)
	}
	if err := h.ArrayStore.PutPendingArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "u", Mountpoint: "/mnt/disk1"},
	}, []store.RecordedDisk{{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sdb", Size: 1}}); err != nil {
		t.Fatal(err)
	}
	m, err := h.GetMigration(ctx)
	if err != nil || m.Phase != apiv1.MigrationPhaseVerifyFailed {
		t.Fatalf("phase = %s, %v, want verify_failed", m.Phase, err)
	}
	v, ok := m.Verify.Get()
	if !ok || v.Status != apiv1.MigrationVerifyStatusFailed || len(v.Disks) != 1 || v.Disks[0].Passed || v.Disks[0].SizeChanged.Paths[0] != "media/a.txt" ||
		v.Disks[0].Found.Bytes != 25 || v.Disks[0].Hashed != 3 || len(v.Shares) != 1 || !v.Shares[0].Passed || v.Duplicates != 1 || v.DuplicateSample[0].Disks[1] != "disk2" || !v.FinishedAt.IsSet() {
		t.Errorf("verify = %+v", v)
	}

	for status, phase := range map[string]apiv1.MigrationPhase{
		migrate.VerifyPassed: apiv1.MigrationPhaseVerified, migrate.VerifyRunning: apiv1.MigrationPhaseVerifying,
	} {
		result.Status = status
		put(result)
		if m, err := h.GetMigration(ctx); err != nil || m.Phase != phase {
			t.Errorf("a %s verify gives phase %s, %v, want %s", status, m.Phase, err, phase)
		}
	}
}
