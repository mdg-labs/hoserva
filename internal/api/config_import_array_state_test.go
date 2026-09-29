package api

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

func arrayMaintenanceRow(t *testing.T, db *sql.DB) (maintenance, stopped int, found bool) {
	t.Helper()
	err := db.QueryRow(`SELECT maintenance, array_stopped FROM array_maintenance WHERE id = 1`).Scan(&maintenance, &stopped)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false
	}
	if err != nil {
		t.Fatalf("reading array_maintenance: %v", err)
	}
	return maintenance, stopped, true
}

// A user who stopped the array for a disk swap and then restores an archive
// taken in normal operation must still have a stopped array afterwards:
// in the database, in the running scheduler, and in a scheduler started
// after a restart. The archive holds no array_maintenance row, so a page-for-page
// restore would read as normal operation and admit every job type again.
func TestImportConfig_KeepsAStoppedArrayStopped(t *testing.T) {
	ctx := context.Background()
	h, registry, db, _ := newImportTestHandlerWithRegistry(t)
	registry.Register(job.TypeMover, true, func(ctx context.Context, rc *job.RunContext) error { return nil })
	archive := exportBytes(t, h)

	if err := h.Scheduler.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	h.Scheduler.MarkArrayStopped()
	if m, s, ok := arrayMaintenanceRow(t, db); !ok || m != 1 || s != 1 {
		t.Fatalf("precondition: array_maintenance = (%d, %d, found %v), want the stopped array persisted", m, s, ok)
	}

	if err := h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}

	if m, s, ok := arrayMaintenanceRow(t, db); !ok || m != 1 || s != 1 {
		t.Fatalf("array_maintenance after import = (%d, %d, found %v), want maintenance=1 array_stopped=1", m, s, ok)
	}
	if !h.Scheduler.InMaintenance() {
		t.Fatal("scheduler left maintenance mode after the import")
	}
	if _, err := h.Scheduler.Submit(ctx, job.TypeMover, nil, nil); !errors.Is(err, job.ErrMaintenanceMode) {
		t.Fatalf("Submit of a data-disk job after the import = %v, want ErrMaintenanceMode", err)
	}

	restarted := job.NewScheduler(job.NewStore(db), job.NewLogStore(t.TempDir()), job.NewHub(), job.NewRegistry())
	if err := restarted.RestorePersistedMaintenance(ctx); err != nil {
		t.Fatalf("RestorePersistedMaintenance: %v", err)
	}
	if !restarted.InMaintenance() {
		t.Fatal("a scheduler started after the import is not in maintenance mode")
	}
}

// The reverse: an archive taken while the array was stopped must not stop a
// running array. No live row means normal operation, and it stays no row.
func TestImportConfig_DoesNotStopARunningArray(t *testing.T) {
	ctx := context.Background()
	h, registry, db, _ := newImportTestHandlerWithRegistry(t)
	registry.Register(job.TypeMover, true, func(ctx context.Context, rc *job.RunContext) error { return nil })
	execAll(t, db, `INSERT INTO array_maintenance (id, maintenance, array_stopped, updated_at) VALUES (1, 1, 1, 't')`)
	archive := exportBytes(t, h)
	execAll(t, db, `DELETE FROM array_maintenance`)

	if err := h.ImportConfig(ctx, importReq(archive)); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}

	if m, s, ok := arrayMaintenanceRow(t, db); ok {
		t.Fatalf("array_maintenance after import = (%d, %d), want no row", m, s)
	}
	if h.Scheduler.InMaintenance() {
		t.Fatal("scheduler entered maintenance mode over the import")
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit after the import = %v, want the job admitted", err)
	}
	if _, err := h.Scheduler.Await(ctx, j.ID); err != nil {
		t.Fatalf("Await: %v", err)
	}
}

// When the live array state cannot be read the import is refused before
// anything is written: the live database keeps the configuration it had and
// the restore hold is released.
func TestImportConfig_RefusesWhenTheLiveArrayStateCannotBeKept(t *testing.T) {
	ctx := context.Background()
	h, registry, db, _ := newImportTestHandlerWithRegistry(t)
	registry.Register(job.TypeSync, true, func(ctx context.Context, rc *job.RunContext) error { return nil })
	archive := exportBytes(t, h)
	insertSentinelShare(t, db, "created-since")
	before := liveFingerprint(t, db)
	execAll(t, db, `ALTER TABLE array_maintenance RENAME TO array_maintenance_gone`)

	err := h.ImportConfig(ctx, importReq(archive))
	if err == nil {
		t.Fatal("ImportConfig succeeded although the live array state could not be read")
	}
	var ae *apiError
	if errors.As(err, &ae) {
		t.Fatalf("err = %v, want an internal error, not a refusal the caller can act on", err)
	}

	if _, err := store.NewShareStore(db).Get(ctx, "created-since"); err != nil {
		t.Fatalf("the live share is gone after a refused import: %v", err)
	}
	if got := liveFingerprint(t, db); got != before {
		t.Fatalf("live database changed by a refused import:\n before %s\n after  %s", before, got)
	}
	execAll(t, db, `ALTER TABLE array_maintenance_gone RENAME TO array_maintenance`)
	j, err := h.Scheduler.Submit(ctx, job.TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit after a refused import = %v, want the restore hold released", err)
	}
	if _, err := h.Scheduler.Await(ctx, j.ID); err != nil {
		t.Fatalf("Await: %v", err)
	}
}

// The preview reports that the live array state is kept, and does not list
// the difference between the two as a change.
func TestPreviewConfigImport_ReportsArrayStateKept(t *testing.T) {
	h, db, _ := newImportTestHandler(t)
	archive := exportBytes(t, h)
	execAll(t, db, `INSERT INTO array_maintenance (id, maintenance, array_stopped, updated_at) VALUES (1, 1, 1, 't')`)

	p, err := h.PreviewConfigImport(context.Background(), previewReq(archive))
	if err != nil {
		t.Fatalf("PreviewConfigImport: %v", err)
	}
	for _, g := range p.Groups {
		if n := len(g.Added) + len(g.Changed) + len(g.Removed); n != 0 {
			t.Fatalf("%s reports %+v, want the array state left out", g.Category, g)
		}
	}
	var codes []apiv1.ConfigImportNoteCode
	for _, n := range p.Notes {
		codes = append(codes, n.Code)
	}
	want := []apiv1.ConfigImportNoteCode{apiv1.ConfigImportNoteCodeSessionsReplaced, apiv1.ConfigImportNoteCodeArrayStateKept}
	if len(codes) != len(want) || codes[0] != want[0] || codes[1] != want[1] {
		t.Fatalf("note codes = %v, want %v", codes, want)
	}
}
