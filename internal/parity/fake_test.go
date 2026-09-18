package parity

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestFakeEngine_Diff_Scripted(t *testing.T) {
	f := NewFakeEngine()
	f.SetDiff(DiffReport{
		Added:   3,
		Removed: 600,
		PerDisk: map[string]DiskDiff{
			"/mnt/disk3": {FilesBefore: 1200, FilesAfter: 0},
		},
	})

	got, err := f.Diff(context.Background())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got.Removed != 600 || got.PerDisk["/mnt/disk3"].FilesAfter != 0 {
		t.Fatalf("Diff: got %+v", got)
	}
}

func TestFakeEngine_Diff_Fails(t *testing.T) {
	f := NewFakeEngine()
	wantErr := errors.New("snapraid: missing content file")
	f.FailDiff(wantErr)

	if _, err := f.Diff(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Diff: got %v, want %v", err, wantErr)
	}
}

func TestFakeEngine_Diff_ContextCancelled(t *testing.T) {
	f := NewFakeEngine()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.Diff(ctx); err == nil {
		t.Fatal("Diff with a cancelled context: got nil error")
	}
}

func TestFakeEngine_Status_Scripted(t *testing.T) {
	f := NewFakeEngine()
	want := ParityStatus{Freshness: FreshnessAmber, ChangedSinceSync: 42, DataDisks: 5, ParityDisks: 1}
	f.SetStatus(want)

	got, err := f.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Status: got %+v, want %+v", got, want)
	}
}

func TestFakeEngine_Status_Fails(t *testing.T) {
	f := NewFakeEngine()
	wantErr := errors.New("snapraid: content file unreadable")
	f.FailStatus(wantErr)

	if _, err := f.Status(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Status: got %v, want %v", err, wantErr)
	}
}

func TestFakeEngine_Sync_StreamsProgressAndCloses(t *testing.T) {
	f := NewFakeEngine()
	f.Sleep = func(time.Duration) {}
	steps := []Progress{
		{Phase: "syncing", Percent: 0},
		{Phase: "syncing", Percent: 50},
		{Phase: "syncing", Percent: 100},
	}
	f.ScriptSync(steps, nil)

	ch, err := f.Sync(context.Background(), SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	var got []Progress
	for p := range ch {
		got = append(got, p)
	}
	if len(got) != len(steps) {
		t.Fatalf("Sync: got %d progress messages, want %d", len(got), len(steps))
	}
	if got[len(got)-1].Percent != 100 {
		t.Fatalf("Sync: last message %+v, want Percent=100", got[len(got)-1])
	}
}

func TestFakeEngine_Sync_ImmediateError(t *testing.T) {
	f := NewFakeEngine()
	wantErr := errors.New("sync: blocked by another job class")
	f.ScriptSync(nil, wantErr)

	ch, err := f.Sync(context.Background(), SyncOpts{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Sync: got err %v, want %v", err, wantErr)
	}
	if ch != nil {
		t.Fatal("Sync with an immediate error: got a non-nil channel")
	}
}

func TestFakeEngine_Sync_FailsMidStream(t *testing.T) {
	f := NewFakeEngine()
	f.Sleep = func(time.Duration) {}
	failErr := errors.New("disk /dev/sdc disappeared mid-sync")
	steps := []Progress{
		{Phase: "syncing", Percent: 0},
		{Phase: "syncing", Percent: 40},
		{Phase: "syncing", Percent: 40, Err: failErr},
	}
	f.ScriptSync(steps, nil)

	ch, err := f.Sync(context.Background(), SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	var last Progress
	count := 0
	for p := range ch {
		last = p
		count++
	}
	if count != len(steps) {
		t.Fatalf("Sync: got %d messages, want %d", count, len(steps))
	}
	if !errors.Is(last.Err, failErr) {
		t.Fatalf("Sync: final message err = %v, want %v", last.Err, failErr)
	}
}

// TestFakeEngine_Sync_GuardBlockRefusesWithoutConfirm confirms FakeEngine
// can scriptably reproduce the one behavior this package exists to
// guarantee (CLAUDE.md): a caller above Engine that's supposed to check
// for *GuardBlockedError and refuse to proceed can now have that path
// exercised against a fast, hardware-free fake, not only against a real
// SnapraidEngine or the lab.
func TestFakeEngine_Sync_GuardBlockRefusesWithoutConfirm(t *testing.T) {
	f := NewFakeEngine()
	f.Sleep = func(time.Duration) {}
	result := GuardResult{Blocked: true, Triggers: []GuardTrigger{TriggerZeroFiles}}
	f.ScriptGuardBlock(result)
	f.ScriptSync([]Progress{{Phase: "syncing", Percent: 100}}, nil)

	ch, err := f.Sync(context.Background(), SyncOpts{})
	if ch != nil {
		t.Fatal("Sync: got a non-nil channel for a blocked sync")
	}
	var blocked *GuardBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Sync: got %v, want a *GuardBlockedError", err)
	}
	if !errors.Is(err, ErrGuardBlocked) {
		t.Fatalf("Sync error %v does not match ErrGuardBlocked", err)
	}
	if !blocked.Result.hasTrigger(TriggerZeroFiles) {
		t.Fatalf("GuardBlockedError.Result.Triggers = %v, want TriggerZeroFiles", blocked.Result.Triggers)
	}
}

// TestFakeEngine_Sync_ConfirmBypassesGuardBlock is the human-decision
// half: SyncOpts.Confirm proceeds past a scripted block, exactly as a
// real caller reviewing the blocked result and retrying would.
func TestFakeEngine_Sync_ConfirmBypassesGuardBlock(t *testing.T) {
	f := NewFakeEngine()
	f.Sleep = func(time.Duration) {}
	f.ScriptGuardBlock(GuardResult{Blocked: true})
	steps := []Progress{{Phase: "syncing", Percent: 100}}
	f.ScriptSync(steps, nil)

	ch, err := f.Sync(context.Background(), SyncOpts{Confirm: true})
	if err != nil {
		t.Fatalf("Sync with Confirm: %v", err)
	}
	var got []Progress
	for p := range ch {
		got = append(got, p)
	}
	if len(got) != len(steps) {
		t.Fatalf("Sync with Confirm: got %d messages, want %d — the scripted steps must still stream", len(got), len(steps))
	}
}

func TestFakeEngine_Scrub_Scripted(t *testing.T) {
	f := NewFakeEngine()
	f.Sleep = func(time.Duration) {}
	f.ScriptScrub([]Progress{{Phase: "scrubbing", Percent: 100}}, nil)

	ch, err := f.Scrub(context.Background(), 8, DefaultScrubOlderThanDays)
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	var got []Progress
	for p := range ch {
		got = append(got, p)
	}
	if len(got) != 1 || got[0].Phase != "scrubbing" {
		t.Fatalf("Scrub: got %+v", got)
	}
}

func TestFakeEngine_Sync_RespectsContextCancellation(t *testing.T) {
	f := NewFakeEngine()
	blocked := make(chan struct{})
	f.Sleep = func(time.Duration) { <-blocked }
	f.ScriptSync([]Progress{
		{Phase: "syncing", Percent: 0},
		{Phase: "syncing", Percent: 50},
		{Phase: "syncing", Percent: 100},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := f.Sync(ctx, SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	first, ok := <-ch
	if !ok || first.Percent != 0 {
		t.Fatalf("Sync: first message = %+v, ok=%v", first, ok)
	}
	cancel()
	close(blocked)

	for range ch {
		// Drain until closed; the goroutine must exit once ctx is done
		// rather than deliver every scripted step.
	}
}

func TestFakeEngine_ImplementsEngine(t *testing.T) {
	var _ Engine = NewFakeEngine()
}
