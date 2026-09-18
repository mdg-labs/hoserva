package parity

import (
	"context"
	"errors"
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
	if got != want {
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
