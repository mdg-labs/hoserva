package api_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/parity"
)

// TestHandler_GetParity_NilConcreteEngine and
// TestHandler_RunParityDiff_NilConcreteEngine reproduce #226: a nil
// *parity.SnapraidEngine wrapped in the parity.Engine interface (exactly
// how cmd/hoservad wires Handler.Parity when no snapraid.conf exists yet)
// does not compare equal to a bare nil, so `h.Parity == nil` used to miss
// it and calling eng.Status/eng.Diff on it panicked — the same pattern
// #222 fixed for /doctor.
func TestHandler_GetParity_NilConcreteEngine(t *testing.T) {
	h, _, _ := newTestHandler(t)
	h.Parity = (*parity.FakeEngine)(nil)

	_, err := h.GetParity(context.Background())
	if ae := apiError(t, h, err); ae.Response.Code != "not_configured" || ae.StatusCode != 501 {
		t.Fatalf("error = %+v, want not_configured/501", ae)
	}
}

func TestHandler_RunParityDiff_NilConcreteEngine(t *testing.T) {
	h, _, _ := newTestHandler(t)
	h.Parity = (*parity.FakeEngine)(nil)

	_, err := h.RunParityDiff(context.Background())
	if ae := apiError(t, h, err); ae.Response.Code != "not_configured" || ae.StatusCode != 501 {
		t.Fatalf("error = %+v, want not_configured/501", ae)
	}
}

type parityMethodRecorder struct {
	parity.FakeEngine

	diffCalled   bool
	statusCalled bool
}

func (r *parityMethodRecorder) Diff(ctx context.Context) (parity.DiffReport, error) {
	r.diffCalled = true
	return r.FakeEngine.Diff(ctx)
}

func (r *parityMethodRecorder) Status(ctx context.Context) (parity.ParityStatus, error) {
	r.statusCalled = true
	return r.FakeEngine.Status(ctx)
}

func TestHandler_GetParity_StatusWithoutDiff(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)
	rec := &parityMethodRecorder{FakeEngine: *parity.NewFakeEngine()}
	rec.SetStatus(parity.ParityStatus{
		Freshness:        parity.FreshnessAmber,
		LastSyncAt:       time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		ChangedSinceSync: 7,
		DataDisks:        3,
		ParityDisks:      1,
	})
	h.Parity = rec

	got, err := h.GetParity(ctx)
	if err != nil {
		t.Fatalf("GetParity: %v", err)
	}
	if !rec.statusCalled {
		t.Fatal("GetParity did not call Status")
	}
	if rec.diffCalled {
		t.Fatal("GetParity called Diff — GET /parity must not wake data disks")
	}
	if got.Freshness != apiv1.ParityFreshnessAmber {
		t.Fatalf("freshness = %q, want amber", got.Freshness)
	}
	if v, ok := got.ChangedSinceSync.Get(); !ok || v != 7 {
		t.Fatalf("changedSinceSync = %+v, want 7", got.ChangedSinceSync)
	}
	if got.Guard.Set {
		t.Fatal("guard present before any run-diff, want omitted")
	}
}

func TestHandler_RunParityDiff_ReturnsGroupedFiles(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)
	rec := parity.NewFakeEngine()
	rec.SetDiff(parity.DiffReport{
		Removed: 2,
		Updated: 1,
		Added:   3,
		Moved:   1,
		Copied:  1,
		RemovedFiles: []parity.DiffFile{
			{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"},
			{Disk: "/mnt/disk2", RelPath: "tv/b.mkv"},
		},
		AddedFiles: []parity.DiffFile{
			{Disk: "/mnt/disk1", RelPath: "movies/c.mkv"},
		},
	})
	h.Parity = rec

	got, err := h.RunParityDiff(ctx)
	if err != nil {
		t.Fatalf("RunParityDiff: %v", err)
	}
	if len(got.Groups) != 6 {
		t.Fatalf("groups = %d, want 6 doc 02 categories", len(got.Groups))
	}
	if got.Groups[0].Category != apiv1.ParityDiffCategoryRemoved || got.Groups[0].Count != 2 {
		t.Fatalf("removed group = %+v, want count 2", got.Groups[0])
	}
	wantRemoved := filepath.Join("/mnt/disk1", "movies/a.mkv")
	if len(got.Groups[0].Paths) != 2 || got.Groups[0].Paths[0] != wantRemoved {
		t.Fatalf("removed paths = %v, want to include %q", got.Groups[0].Paths, wantRemoved)
	}
	if got.Groups[1].Category != apiv1.ParityDiffCategoryUpdated || got.Groups[1].Count != 1 {
		t.Fatalf("updated group = %+v", got.Groups[1])
	}
}

func TestHandler_RunParityDiff_CachesGuardAnnotatedDiff(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)
	rec := parity.NewFakeEngine()
	rec.SetDiff(parity.DiffReport{
		Removed:        2,
		MovedByHoserva: 9,
		RemovedFiles: []parity.DiffFile{
			{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"},
			{Disk: "/mnt/disk2", RelPath: "tv/b.mkv"},
		},
	})
	h.Parity = rec

	got, err := h.RunParityDiff(ctx)
	if err != nil {
		t.Fatalf("RunParityDiff: %v", err)
	}
	if got.Groups[5].Category != apiv1.ParityDiffCategoryMovedByHoserva || got.Groups[5].Count != 0 {
		t.Fatalf("moved-by-hoserva = %+v, want count 0 from guard.Diff when no relocation manifest is loaded", got.Groups[5])
	}
	if got.Groups[0].Count != 2 {
		t.Fatalf("removed count = %d, want 2 (unmodified by a nil manifest)", got.Groups[0].Count)
	}
}

func TestHandler_RunParityDiff_TrippedGuardWithoutSync(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)
	rec := parity.NewFakeEngine()
	rec.SetDiff(parity.DiffReport{
		Removed: 600,
		PerDisk: map[string]parity.DiskDiff{
			"/mnt/disk1": {FilesBefore: 10000, FilesAfter: 9400},
		},
	})
	h.Parity = rec

	got, err := h.RunParityDiff(ctx)
	if err != nil {
		t.Fatalf("RunParityDiff: %v", err)
	}
	if !got.Guard.WouldBlock {
		t.Fatal("guard.wouldBlock = false, want true without starting a sync")
	}
	if len(got.Guard.Triggers) == 0 || got.Guard.Triggers[0] != apiv1.ParityGuardTriggerRemovedCount {
		t.Fatalf("guard.triggers = %v, want removed-count", got.Guard.Triggers)
	}
	if summary, ok := got.Guard.Summary.Get(); !ok || summary == "" {
		t.Fatalf("guard.summary = %+v, want a plain-language reason", got.Guard.Summary)
	}

	cached, err := h.GetParity(ctx)
	if err != nil {
		t.Fatalf("GetParity after run-diff: %v", err)
	}
	if !cached.Guard.Set || !cached.Guard.Value.WouldBlock {
		t.Fatalf("cached guard = %+v, want wouldBlock true on GET /parity", cached.Guard)
	}
}
