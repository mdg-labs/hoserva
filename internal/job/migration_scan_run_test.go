package job

import (
	"context"
	"io"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func TestMigrationScan_IsATopologyJobThatTakesNoPreTopologyBackup(t *testing.T) {
	ctx := context.Background()
	if class, ok := ClassOf(TypeMigrationScan); !ok || class != ClassTopology {
		t.Fatalf("ClassOf(migration_scan) = %v, %v, want topology", class, ok)
	}
	s := newTestScheduler(t)
	var upload string
	s.registry.Register(TypeMigrationScan, false, RunMigrationScan(func(_ context.Context, _ io.Writer, u string) error {
		upload = u
		return nil
	}))
	backup := &fakeBackup{}
	s.SetTopologyBackup(backup)

	j, err := s.Submit(ctx, TypeMigrationScan, []string{"migration"}, mustJSON(t, MigrationScanParams{Upload: "upload-abc.zip"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if done := await(t, s, j.ID); done.Status != StatusSucceeded {
		t.Fatalf("scan job = %s %s", done.Status, done.ErrorMessage)
	}
	if upload != "upload-abc.zip" {
		t.Errorf("the run was given upload %q, want the one in the job's params", upload)
	}
	if got := backup.count(); got != 0 {
		t.Errorf("a read-only scan ran the pre-topology backup %d times, want 0", got)
	}
}

func TestMigrationScan_QueuedBehindATopologyJobStillTakesNoBackup(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	started, release := registerBlocking(s, TypeDiskAdd, false)
	noopRun(s, TypeMigrationScan, false)
	backup := &fakeBackup{}
	s.SetTopologyBackup(backup)

	add, err := s.Submit(ctx, TypeDiskAdd, nil, mustJSON(t, DiskAddParams{Confirmation: "confirm", Disk: disk.AssignedDisk{Device: "/dev/sdz"}}))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	scan, err := s.Submit(ctx, TypeMigrationScan, []string{"migration"}, mustJSON(t, MigrationScanParams{Upload: "u.zip"}))
	if err != nil {
		t.Fatal(err)
	}
	if scan.Status != StatusQueued {
		t.Fatalf("scan beside a running topology job = %s, want queued", scan.Status)
	}
	close(release)
	await(t, s, add.ID)
	if done := await(t, s, scan.ID); done.Status != StatusSucceeded {
		t.Fatalf("scan = %s", done.Status)
	}
	if got := backup.count(); got != 1 {
		t.Errorf("pre-topology backup ran %d times, want only the disk add's one", got)
	}
}

// A storage job never runs beside a scan, which is why it is a Topology job.
func TestMigrationScan_ExcludesStorageJobs(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	started, release := registerBlocking(s, TypeMigrationScan, false)
	noopRun(s, TypeSync, true)

	scan, err := s.Submit(ctx, TypeMigrationScan, []string{"migration"}, mustJSON(t, MigrationScanParams{Upload: "u.zip"}))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	sync, err := s.Submit(ctx, TypeSync, nil, mustJSON(t, SyncParams{Confirm: true}))
	if err != nil {
		t.Fatal(err)
	}
	if sync.Status != StatusQueued {
		t.Fatalf("sync beside a running scan = %s, want queued", sync.Status)
	}
	close(release)
	await(t, s, scan.ID)
	await(t, s, sync.ID)
}

func TestValidateParams_MigrationScanNeedsAnUpload(t *testing.T) {
	for name, params := range map[string][]byte{"none": nil, "null": []byte("null"), "empty upload": []byte(`{"upload":""}`), "not json": []byte("x"), "unknown field": []byte(`{"upload":"a","x":1}`)} {
		if err := ValidateParams(TypeMigrationScan, params); err == nil {
			t.Errorf("%s: ValidateParams accepted %q", name, params)
		}
	}
	if err := ValidateParams(TypeMigrationScan, []byte(`{"upload":"upload-1.zip"}`)); err != nil {
		t.Errorf("a valid payload: %v", err)
	}
}
