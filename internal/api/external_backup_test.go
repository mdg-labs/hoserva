package api_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

type externalBackupRig struct {
	h       *api.Handler
	db      *sql.DB
	svc     *backup.Service
	extRoot string
	mounted map[string]bool
}

// newExternalBackupRig wires the external-disk handlers to a backup.Service
// on the same migrated database, the way cmd/hoservad does.
func newExternalBackupRig(t *testing.T) *externalBackupRig {
	t.Helper()
	db := openTestDB(t)
	root := t.TempDir()
	rig := &externalBackupRig{db: db, extRoot: filepath.Join(root, "disks"), mounted: map[string]bool{}}
	rig.svc = &backup.Service{
		DB:           db,
		Store:        api.NewBackupDestinationStore(db),
		ExternalRoot: rig.extRoot,
		ExternalMounted: func(_ context.Context, path string) (bool, error) {
			return rig.mounted[path], nil
		},
	}
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, Boot: true})
	p.AddDisk("/dev/sde", disk.Disk{Size: 4 * disk.TB, Filesystem: "xfs", Label: "backup", FSUUID: "uuid-ext", Serial: "EXT1"})
	rig.h = &api.Handler{
		ArrayStore: store.NewArrayStore(db),
		Disks:      p,
		DiskRunner: disk.NewFakeRunner(),
		Backup:     rig.svc,
	}
	return rig
}

func (r *externalBackupRig) flag(t *testing.T, label string, on bool) (*apiv1.ExternalDisk, error) {
	t.Helper()
	req := &apiv1.UpdateExternalDiskRequest{}
	req.SetBackupDestination(apiv1.NewOptBool(on))
	return r.h.UpdateExternalDisk(context.Background(), req, apiv1.UpdateExternalDiskParams{Label: apiv1.ExternalDiskLabel(label)})
}

func (r *externalBackupRig) destinationIDs(t *testing.T) []string {
	t.Helper()
	list, err := r.h.ListBackupDestinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range list.Destinations {
		ids = append(ids, d.ID)
	}
	return ids
}

func (r *externalBackupRig) storedFlag(t *testing.T, label string) bool {
	t.Helper()
	row, err := store.NewExternalStore(r.db).GetExternalDisk(context.Background(), label)
	if err != nil {
		t.Fatalf("GetExternalDisk(%s): %v", label, err)
	}
	return row.BackupDestination
}

func TestUpdateExternalDisk_FlagCreatesAndRemovesTheBackupDestination(t *testing.T) {
	ctx := context.Background()
	rig := newExternalBackupRig(t)

	got, err := rig.flag(t, "backup", true)
	if err != nil || !got.BackupDestination {
		t.Fatalf("flag on = %+v, %v", got, err)
	}
	list, err := rig.h.ListBackupDestinations(ctx)
	if err != nil || len(list.Destinations) != 1 {
		t.Fatalf("destinations after flag on = %+v, %v", list, err)
	}
	d := list.Destinations[0]
	if d.ID != "external:backup" || d.Path != "/mnt/disks/backup" || d.Type != apiv1.BackupDestinationTypeLocal || !d.Enabled {
		t.Fatalf("destination = %+v", d)
	}
	if d.Retention.Daily != backup.DefaultRetentionDaily {
		t.Fatalf("retention = %+v, want the defaults", d.Retention)
	}

	if _, err := rig.flag(t, "backup", true); err != nil {
		t.Fatalf("flag on twice: %v", err)
	}
	if ids := rig.destinationIDs(t); len(ids) != 1 {
		t.Fatalf("flag on twice left destinations %v", ids)
	}

	if got, err = rig.flag(t, "backup", false); err != nil || got.BackupDestination {
		t.Fatalf("flag off = %+v, %v", got, err)
	}
	if ids := rig.destinationIDs(t); len(ids) != 0 {
		t.Fatalf("flag off left destinations %v", ids)
	}
	if rig.storedFlag(t, "backup") {
		t.Fatal("flag off left the stored flag set")
	}
}

func TestRegisterExternalDisk_WithFlagCreatesTheBackupDestination(t *testing.T) {
	rig := newExternalBackupRig(t)
	got, err := rig.h.RegisterExternalDisk(context.Background(), &apiv1.RegisterExternalDiskRequest{
		Device: "/dev/sde", Label: "backup", BackupDestination: apiv1.NewOptBool(true),
	})
	if err != nil || !got.BackupDestination {
		t.Fatalf("RegisterExternalDisk = %+v, %v", got, err)
	}
	if ids := rig.destinationIDs(t); len(ids) != 1 || ids[0] != "external:backup" {
		t.Fatalf("destinations = %v, want external:backup", ids)
	}

	other, err := newExternalBackupRig(t).h.RegisterExternalDisk(context.Background(), &apiv1.RegisterExternalDiskRequest{Device: "/dev/sde", Label: "backup"})
	if err != nil || other.BackupDestination {
		t.Fatalf("registering without the flag = %+v, %v", other, err)
	}
}

func TestRegisterExternalDisk_RefusedDestinationRegistersNothing(t *testing.T) {
	ctx := context.Background()
	rig := newExternalBackupRig(t)
	_, err := rig.h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{Name: "Backup", Type: apiv1.BackupDestinationTypeLocal, Path: "/srv/elsewhere"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = rig.h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{
		Device: "/dev/sde", Label: "backup", BackupDestination: apiv1.NewOptBool(true),
	})
	if ae := apiError(t, rig.h, err); ae.StatusCode != 409 || ae.Response.Code != "backup_destination_exists" {
		t.Fatalf("status = %d %q, want 409 backup_destination_exists", ae.StatusCode, ae.Response.Code)
	}
	if _, err := store.NewExternalStore(rig.db).GetExternalDisk(ctx, "backup"); err == nil {
		t.Fatal("the disk was registered although its destination was refused")
	}
}

func TestUpdateExternalDisk_NameClashLeavesFlagAndDestinationsUnchanged(t *testing.T) {
	ctx := context.Background()
	rig := newExternalBackupRig(t)
	if _, err := rig.h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sde", Label: "backup"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.h.CreateBackupDestination(ctx, &apiv1.CreateBackupDestinationRequest{Name: "BACKUP", Type: apiv1.BackupDestinationTypeLocal, Path: "/srv/elsewhere"}); err != nil {
		t.Fatal(err)
	}

	_, err := rig.flag(t, "backup", true)
	if ae := apiError(t, rig.h, err); ae.StatusCode != 409 || ae.Response.Code != "backup_destination_exists" {
		t.Fatalf("status = %d %q, want 409 backup_destination_exists", ae.StatusCode, ae.Response.Code)
	}
	if rig.storedFlag(t, "backup") {
		t.Fatal("a refused flag was stored")
	}
	if ids := rig.destinationIDs(t); len(ids) != 1 {
		t.Fatalf("destinations = %v, want only the one that clashed", ids)
	}
}

func TestDeleteBackupDestination_ExternalClearsTheDiskFlag(t *testing.T) {
	ctx := context.Background()
	rig := newExternalBackupRig(t)
	if _, err := rig.flag(t, "backup", true); err != nil {
		t.Fatal(err)
	}

	if err := rig.h.DeleteBackupDestination(ctx, apiv1.DeleteBackupDestinationParams{DestinationId: "external:backup"}); err != nil {
		t.Fatalf("DeleteBackupDestination: %v", err)
	}
	if rig.storedFlag(t, "backup") {
		t.Fatal("deleting the destination left the disk's flag set")
	}
	list, err := rig.h.ListExternalDisks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range list.Disks {
		if d.Label == "backup" && d.BackupDestination {
			t.Fatal("the disk still reports backupDestination")
		}
	}

	err = rig.h.DeleteBackupDestination(ctx, apiv1.DeleteBackupDestinationParams{DestinationId: "external:backup"})
	if ae := apiError(t, rig.h, err); ae.StatusCode != 404 {
		t.Fatalf("second delete status = %d, want 404", ae.StatusCode)
	}
}

// failOn makes SQLite refuse the statement that follows the first write of
// a two-table change, so each test proves the first write was rolled back.
func failOn(t *testing.T, db *sql.DB, trigger string) {
	t.Helper()
	if _, err := db.Exec(trigger); err != nil {
		t.Fatalf("creating failure trigger: %v", err)
	}
}

func TestSetExternalDestination_FailureInTheSecondWriteChangesNeither(t *testing.T) {
	ctx := context.Background()
	rig := newExternalBackupRig(t)
	if _, err := rig.h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sde", Label: "backup"}); err != nil {
		t.Fatal(err)
	}
	failOn(t, rig.db, `CREATE TRIGGER fail_dest_insert BEFORE INSERT ON backup_destinations
		BEGIN SELECT RAISE(ABORT, 'injected destination failure'); END`)

	if _, err := rig.flag(t, "backup", true); err == nil || !strings.Contains(err.Error(), "injected destination failure") {
		t.Fatalf("flag on = %v, want the injected failure", err)
	}
	if rig.storedFlag(t, "backup") {
		t.Fatal("the flag stayed set although the destination was not created")
	}
	if ids := rig.destinationIDs(t); len(ids) != 0 {
		t.Fatalf("destinations = %v", ids)
	}
}

func TestPutExternalDisk_FailureCreatingTheDestinationRegistersNothing(t *testing.T) {
	ctx := context.Background()
	rig := newExternalBackupRig(t)
	failOn(t, rig.db, `CREATE TRIGGER fail_dest_insert BEFORE INSERT ON backup_destinations
		BEGIN SELECT RAISE(ABORT, 'injected destination failure'); END`)

	_, err := rig.h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{
		Device: "/dev/sde", Label: "backup", BackupDestination: apiv1.NewOptBool(true),
	})
	if err == nil || !strings.Contains(err.Error(), "injected destination failure") {
		t.Fatalf("RegisterExternalDisk = %v, want the injected failure", err)
	}
	if _, err := store.NewExternalStore(rig.db).GetExternalDisk(ctx, "backup"); err == nil {
		t.Fatal("the disk stayed registered although its destination was not created")
	}
}

func TestSetExternalDestinationOff_FailureDeletingTheDestinationKeepsTheFlag(t *testing.T) {
	rig := newExternalBackupRig(t)
	if _, err := rig.flag(t, "backup", true); err != nil {
		t.Fatal(err)
	}
	failOn(t, rig.db, `CREATE TRIGGER fail_dest_delete BEFORE DELETE ON backup_destinations
		BEGIN SELECT RAISE(ABORT, 'injected destination failure'); END`)

	if _, err := rig.flag(t, "backup", false); err == nil {
		t.Fatal("flag off succeeded although the destination could not be deleted")
	}
	if !rig.storedFlag(t, "backup") {
		t.Fatal("the flag was cleared although the destination stayed")
	}
	if ids := rig.destinationIDs(t); len(ids) != 1 {
		t.Fatalf("destinations = %v, want the destination kept", ids)
	}
}

func TestDeleteBackupDestination_ExternalFailureClearingTheFlagKeepsTheDestination(t *testing.T) {
	ctx := context.Background()
	rig := newExternalBackupRig(t)
	if _, err := rig.flag(t, "backup", true); err != nil {
		t.Fatal(err)
	}
	failOn(t, rig.db, `CREATE TRIGGER fail_flag_update BEFORE UPDATE ON external_disks
		BEGIN SELECT RAISE(ABORT, 'injected flag failure'); END`)

	err := rig.h.DeleteBackupDestination(ctx, apiv1.DeleteBackupDestinationParams{DestinationId: "external:backup"})
	if err == nil || !strings.Contains(err.Error(), "injected flag failure") {
		t.Fatalf("DeleteBackupDestination = %v, want the injected failure", err)
	}
	if ids := rig.destinationIDs(t); len(ids) != 1 {
		t.Fatalf("destinations = %v, want the destination kept", ids)
	}
	if !rig.storedFlag(t, "backup") {
		t.Fatal("the flag changed although the delete failed")
	}
}

func TestTestBackupDestination_ExternalDiskRefusedWhileEjected(t *testing.T) {
	ctx := context.Background()
	rig := newExternalBackupRig(t)
	if _, err := rig.flag(t, "backup", true); err != nil {
		t.Fatal(err)
	}
	// The destination's path is /mnt/disks/backup, outside the rig's root:
	// point it at the rig's own directory, which is not mounted.
	path := filepath.Join(rig.extRoot, "backup")
	if _, err := rig.db.Exec(`UPDATE backup_destinations SET path = ? WHERE id = 'external:backup'`, path); err != nil {
		t.Fatal(err)
	}

	res, err := rig.h.TestBackupDestination(ctx, apiv1.TestBackupDestinationParams{DestinationId: "external:backup"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Success || !strings.Contains(res.Error.Value, "not mounted") {
		t.Fatalf("result = %+v, want a not-mounted refusal", res)
	}
	if _, err := os.Stat(rig.extRoot); !os.IsNotExist(err) {
		t.Fatalf("the test created %q: %v", rig.extRoot, err)
	}
}

func TestExternalFlag_WithoutBackupDestinationsKeepsTheFlagAlone(t *testing.T) {
	h, _, _, _ := newExternalHandler(t)
	ctx := context.Background()
	if _, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sde", Label: "backup", BackupDestination: apiv1.NewOptBool(true)}); err != nil {
		t.Fatalf("RegisterExternalDisk: %v", err)
	}
	req := &apiv1.UpdateExternalDiskRequest{}
	req.SetBackupDestination(apiv1.NewOptBool(false))
	got, err := h.UpdateExternalDisk(ctx, req, apiv1.UpdateExternalDiskParams{Label: "backup"})
	if err != nil || got.BackupDestination {
		t.Fatalf("UpdateExternalDisk = %+v, %v", got, err)
	}
}
