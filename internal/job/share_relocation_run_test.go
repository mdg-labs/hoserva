package job

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/parity"
)

// syncFuncFromEngine adapts a parity.Engine into a cache.SyncFunc the way
// a real daemon's own share-relocation wiring must (cache.SyncFunc's own
// doc comment): draining the returned progress channel with this
// package's own drainProgress (parity_run.go) so a blocked or failed
// sync surfaces as a plain error, exactly as RunSync's engine call does.
func syncFuncFromEngine(e parity.Engine) cache.SyncFunc {
	return func(ctx context.Context, manifest []parity.ManifestEntry) error {
		ch, err := e.Sync(ctx, parity.SyncOpts{Manifest: manifest})
		return drainProgress(ch, err)
	}
}

func newShareRelocationShare(t *testing.T, name string) cache.Share {
	t.Helper()
	base := t.TempDir()
	return cache.Share{
		Name:      name,
		CachePath: filepath.Join(base, "cache", name),
		Branches:  []string{filepath.Join(base, "disk1", name)},
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

// TestRunShareRelocation_ToCache_GuardBlocked_LeavesArrayUntouched is this
// issue's own central safety test (CLAUDE.md: "anything that can lose
// data gets its test before its implementation"): the job wiring this
// issue adds must route RelocateToCache's sync through the real
// threshold guard exactly as internal/cache's own #54 tests already
// prove RelocateToCache itself does — a blocked sync must fail the job
// before anything is deleted from the array, with the cache-side copy
// surviving so a later, confirmed run can still finish.
func TestRunShareRelocation_ToCache_GuardBlocked_LeavesArrayUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	src := filepath.Join(share.Branches[0], "report.pdf")
	mustWriteFile(t, src, "report bytes")

	eng := newRecordingEngine()
	eng.ScriptGuardBlock(trippedGuard())

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Share: func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:  syncFuncFromEngine(eng),
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}
	if !strings.Contains(finished.ErrorMessage, "threshold guard blocked the sync") {
		t.Fatalf("ErrorMessage = %q, want a threshold-guard block", finished.ErrorMessage)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("array original must survive a blocked sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(share.CachePath, "report.pdf")); err != nil {
		t.Fatalf("verified cache copy must survive a blocked sync: %v", err)
	}
	_, wrote, _, _ := eng.snapshot()
	if wrote {
		t.Fatal("sync proceeded past a tripped threshold guard — parity would have been written over the deletions")
	}
}

// TestRunShareRelocation_ToCache_MovesThroughScheduler proves the happy
// path reaches RelocateToCache through a real Scheduler.Submit/Await
// round trip: the array-side file is copied to cache, synced and its
// array original removed (Q14).
func TestRunShareRelocation_ToCache_MovesThroughScheduler(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	share := newShareRelocationShare(t, "docs")
	src := filepath.Join(share.Branches[0], "report.pdf")
	mustWriteFile(t, src, "report bytes")

	eng := newRecordingEngine()
	eng.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Share: func(context.Context, string) (cache.Share, error) { return share, nil },
		Sync:  syncFuncFromEngine(eng),
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	got, err := os.ReadFile(filepath.Join(share.CachePath, "report.pdf"))
	if err != nil || string(got) != "report bytes" {
		t.Fatalf("cache content = %q, %v, want %q", got, err, "report bytes")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("array original should be gone after relocation: err=%v", err)
	}
}

// TestRunShareRelocation_ToArray_MovesThroughScheduler proves the
// cache-to-array direction reaches RelocateToArray through a real
// scheduler round trip, ignoring the grace period the way #54's own
// RelocateToArray always does.
func TestRunShareRelocation_ToArray_MovesThroughScheduler(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	base := t.TempDir()
	share := cache.Share{
		Name:      "docs",
		CachePath: filepath.Join(base, "cache", "docs"),
		ArrayPath: filepath.Join(base, "array", "docs"),
	}
	src := filepath.Join(share.CachePath, "report.pdf")
	mustWriteFile(t, src, "report bytes")

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Share: func(context.Context, string) (cache.Share, error) { return share, nil },
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "docs", To: "array"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	dst := filepath.Join(share.ArrayPath, "report.pdf")
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "report bytes" {
		t.Fatalf("array content = %q, %v, want %q", got, err, "report bytes")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("cache original should be gone after relocation: err=%v", err)
	}
}

// TestRunShareRelocation_UnknownShareFailsTheJob proves a share the
// caller cannot resolve fails the job rather than relocating nothing
// silently — mirroring TestRunMover_SharesErrorFailsTheJob.
func TestRunShareRelocation_UnknownShareFailsTheJob(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	wantErr := errors.New("no such share")

	s.registry.Register(TypeShareRelocation, true, RunShareRelocation(ShareRelocationDeps{
		Share: func(context.Context, string) (cache.Share, error) { return cache.Share{}, wantErr },
	}))

	j, err := s.Submit(ctx, TypeShareRelocation, nil, mustJSON(t, ShareRelocationParams{Share: "ghost", To: "array"}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}
	if !strings.Contains(finished.ErrorMessage, "no such share") {
		t.Fatalf("ErrorMessage = %q, want it to surface the share-resolution failure", finished.ErrorMessage)
	}
}

func TestValidateParams_ShareRelocationRequiresShareAndValidDirection(t *testing.T) {
	for _, params := range [][]byte{
		nil,
		[]byte("null"),
		[]byte(""),
		[]byte(`{"to":"array"}`),
		[]byte(`{"share":"docs"}`),
		[]byte(`{"share":"docs","to":"elsewhere"}`),
	} {
		if err := ValidateParams(TypeShareRelocation, params); err == nil {
			t.Fatalf("ValidateParams(share_relocation, %q) = nil, want rejection", params)
		}
	}
	if err := ValidateParams(TypeShareRelocation, mustJSON(t, ShareRelocationParams{Share: "docs", To: "cache"})); err != nil {
		t.Fatalf("ValidateParams(share_relocation, valid) = %v, want nil", err)
	}
}
