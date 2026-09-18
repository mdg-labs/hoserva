package parity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(time.Millisecond)
	}
}

func pushOne(w *FakeWatcher, mountpoint string, ev ChangeEvent) {
	s := w.Stream(mountpoint)
	s.Push([]StreamEvent{{Change: ev}})
}

func TestJournal_CountsDistinctEntries(t *testing.T) {
	w := NewFakeWatcher()
	j := NewJournal(w, "")
	t.Cleanup(func() { _ = j.Close() })
	ctx := context.Background()

	if err := j.AddDisk(ctx, "disk1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}

	pushOne(w, "/mnt/disk1", ChangeEvent{Kind: ChangeCreate, ID: "dir1/a.txt", Name: "a.txt"})
	pushOne(w, "/mnt/disk1", ChangeEvent{Kind: ChangeCreate, ID: "dir1/b.txt", Name: "b.txt"})
	// A second event on the same entry (e.g. write then close) must not
	// count as a second changed file — CLAUDE.md: the count approximates
	// distinct changed files, not raw event volume.
	pushOne(w, "/mnt/disk1", ChangeEvent{Kind: ChangeCloseWrite, ID: "dir1/a.txt", Name: "a.txt"})

	waitFor(t, time.Second, func() bool {
		s, err := j.Summary("disk1")
		return err == nil && s.Count == 2
	})

	files, err := j.Files("disk1")
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("Files returned %d entries, want 2: %+v", len(files), files)
	}
	names := map[string]ChangeKind{}
	for _, f := range files {
		names[f.Name] = f.Kind
	}
	if names["a.txt"] != ChangeCloseWrite {
		t.Fatalf("a.txt's recorded kind = %v, want ChangeCloseWrite (the later event)", names["a.txt"])
	}
	if _, ok := names["b.txt"]; !ok {
		t.Fatalf("b.txt missing from Files: %+v", files)
	}
}

// TestJournal_Snapshot_MatchesSummaryAndFiles is FixPendingFiles's own
// atomicity requirement (doc 03 §3.5): a single Snapshot call must report
// the same Count as len(Files), never an earlier or later moment than the
// files it lists beside it — the property two separate Summary/Files
// calls can't guarantee under a concurrent journal write.
func TestJournal_Snapshot_MatchesSummaryAndFiles(t *testing.T) {
	w := NewFakeWatcher()
	j := NewJournal(w, "")
	t.Cleanup(func() { _ = j.Close() })
	ctx := context.Background()

	if err := j.AddDisk(ctx, "disk1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}
	pushOne(w, "/mnt/disk1", ChangeEvent{Kind: ChangeCreate, ID: "dir1/a.txt", Name: "a.txt"})
	pushOne(w, "/mnt/disk1", ChangeEvent{Kind: ChangeCreate, ID: "dir1/b.txt", Name: "b.txt"})
	waitFor(t, time.Second, func() bool {
		s, err := j.Summary("disk1")
		return err == nil && s.Count == 2
	})

	summary, files, err := j.Snapshot("disk1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if summary.Count != len(files) {
		t.Fatalf("Snapshot: Count = %d but Files has %d entries — not the same moment", summary.Count, len(files))
	}
	if summary.Count != 2 {
		t.Fatalf("Snapshot: Count = %d, want 2", summary.Count)
	}
}

func TestJournal_Snapshot_UnknownDiskErrors(t *testing.T) {
	w := NewFakeWatcher()
	j := NewJournal(w, "")
	t.Cleanup(func() { _ = j.Close() })

	if _, _, err := j.Snapshot("nope"); !errors.Is(err, ErrDiskNotTracked) {
		t.Fatalf("Snapshot: got %v, want ErrDiskNotTracked", err)
	}
}

func TestJournal_Overflow(t *testing.T) {
	w := NewFakeWatcher()
	j := NewJournal(w, "")
	t.Cleanup(func() { _ = j.Close() })
	ctx := context.Background()

	if err := j.AddDisk(ctx, "disk1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}

	s := w.Stream("/mnt/disk1")
	s.Push([]StreamEvent{{Overflow: true}})

	waitFor(t, time.Second, func() bool {
		sum, err := j.Summary("disk1")
		return err == nil && sum.Overflowed
	})
}

func TestJournal_ResetSince(t *testing.T) {
	w := NewFakeWatcher()
	j := NewJournal(w, "")
	t.Cleanup(func() { _ = j.Close() })
	ctx := context.Background()

	if err := j.AddDisk(ctx, "disk1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}
	pushOne(w, "/mnt/disk1", ChangeEvent{Kind: ChangeCreate, ID: "dir1/a.txt", Name: "a.txt"})
	w.Stream("/mnt/disk1").Push([]StreamEvent{{Overflow: true}})

	waitFor(t, time.Second, func() bool {
		sum, err := j.Summary("disk1")
		return err == nil && sum.Count == 1 && sum.Overflowed
	})

	if err := j.ResetSince("disk1"); err != nil {
		t.Fatalf("ResetSince: %v", err)
	}
	sum, err := j.Summary("disk1")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Count != 0 || sum.Overflowed {
		t.Fatalf("Summary after ResetSince = %+v, want zero count and Overflowed=false", sum)
	}

	files, err := j.Files("disk1")
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("Files after ResetSince = %+v, want empty", files)
	}
}

func TestJournal_UnknownDiskErrors(t *testing.T) {
	j := NewJournal(NewFakeWatcher(), "")

	if _, err := j.Summary("nope"); err == nil {
		t.Fatal("Summary for an untracked disk: got nil error")
	}
	if _, err := j.Files("nope"); err == nil {
		t.Fatal("Files for an untracked disk: got nil error")
	}
	if err := j.ResetSince("nope"); err == nil {
		t.Fatal("ResetSince for an untracked disk: got nil error")
	}
	if err := j.RemoveDisk("nope"); err == nil {
		t.Fatal("RemoveDisk for an untracked disk: got nil error")
	}
	if err := j.Rearm(context.Background(), "nope"); err == nil {
		t.Fatal("Rearm for an untracked disk: got nil error")
	}
}

func TestJournal_DuplicateAddDiskFails(t *testing.T) {
	w := NewFakeWatcher()
	j := NewJournal(w, "")
	t.Cleanup(func() { _ = j.Close() })
	ctx := context.Background()

	if err := j.AddDisk(ctx, "disk1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}
	if err := j.AddDisk(ctx, "disk1", "/mnt/disk1"); err == nil {
		t.Fatal("second AddDisk for the same diskID: got nil error")
	}
}

func TestJournal_RemoveDisk_StopsAndForgets(t *testing.T) {
	w := NewFakeWatcher()
	j := NewJournal(w, "")
	ctx := context.Background()

	if err := j.AddDisk(ctx, "disk1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}
	if err := j.RemoveDisk("disk1"); err != nil {
		t.Fatalf("RemoveDisk: %v", err)
	}
	if _, err := j.Summary("disk1"); err == nil {
		t.Fatal("Summary after RemoveDisk: got nil error, want ErrDiskNotTracked")
	}
}

// TestJournal_RemountStopsListeningSilently mirrors spike S7's own
// finding (doc 08 §7): a filesystem remount stops event delivery with no
// error surfaced to the reader that was already blocked — here modelled
// as the underlying stream simply closing without the disk being
// removed. Summary().Listening must go false so a caller can tell the
// count has gone stale, and Rearm is what resumes it.
func TestJournal_RemountStopsListeningSilently(t *testing.T) {
	w := NewFakeWatcher()
	j := NewJournal(w, "")
	t.Cleanup(func() { _ = j.Close() })
	ctx := context.Background()

	if err := j.AddDisk(ctx, "disk1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}
	pushOne(w, "/mnt/disk1", ChangeEvent{Kind: ChangeCreate, ID: "dir1/a.txt", Name: "a.txt"})
	waitFor(t, time.Second, func() bool {
		s, err := j.Summary("disk1")
		return err == nil && s.Count == 1 && s.Listening
	})

	// Simulate the remount: the stream this disk was reading from goes
	// away with no error, just like a real FAN_MARK_FILESYSTEM mark
	// silently stops delivering once its filesystem instance is gone.
	_ = w.Stream("/mnt/disk1").Close()
	waitFor(t, time.Second, func() bool {
		s, err := j.Summary("disk1")
		return err == nil && !s.Listening
	})

	// The count survives the gap — only Rearm loses nothing recorded so
	// far, and only new events after Rearm are added.
	sum, err := j.Summary("disk1")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Count != 1 {
		t.Fatalf("Summary.Count after a silent remount = %d, want 1 (unchanged)", sum.Count)
	}

	if err := j.Rearm(ctx, "disk1"); err != nil {
		t.Fatalf("Rearm: %v", err)
	}
	waitFor(t, time.Second, func() bool {
		s, err := j.Summary("disk1")
		return err == nil && s.Listening
	})

	pushOne(w, "/mnt/disk1", ChangeEvent{Kind: ChangeCreate, ID: "dir1/b.txt", Name: "b.txt"})
	waitFor(t, time.Second, func() bool {
		s, err := j.Summary("disk1")
		return err == nil && s.Count == 2
	})
}

func TestJournal_PersistsAndReloadsAcrossRestart(t *testing.T) {
	root := t.TempDir()
	w := NewFakeWatcher()
	j := NewJournal(w, root)
	ctx := context.Background()

	if err := j.AddDisk(ctx, "disk1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}
	pushOne(w, "/mnt/disk1", ChangeEvent{Kind: ChangeCreate, ID: "dir1/a.txt", Name: "a.txt", Path: "/mnt/disk1/dir1/a.txt"})

	snapPath := filepath.Join(root, "disk1.json")
	waitFor(t, time.Second, func() bool {
		data, err := os.ReadFile(snapPath)
		if err != nil {
			return false
		}
		var snap journalSnapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			return false
		}
		return len(snap.Entries) == 1
	})

	if err := j.RemoveDisk("disk1"); err != nil {
		t.Fatalf("RemoveDisk: %v", err)
	}

	// A fresh Journal, modelling a hoservad restart: AddDisk must resume
	// from the persisted snapshot rather than starting at zero (doc 02
	// §2: a restart must not silently forget already-counted changes,
	// even though — as the remount test above shows separately — it
	// does lose whatever happens during the restart gap itself).
	w2 := NewFakeWatcher()
	j2 := NewJournal(w2, root)
	t.Cleanup(func() { _ = j2.Close() })
	if err := j2.AddDisk(ctx, "disk1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk after restart: %v", err)
	}

	sum, err := j2.Summary("disk1")
	if err != nil {
		t.Fatalf("Summary after restart: %v", err)
	}
	if sum.Count != 1 {
		t.Fatalf("Summary.Count after restart = %d, want 1 (resumed from snapshot)", sum.Count)
	}

	files, err := j2.Files("disk1")
	if err != nil {
		t.Fatalf("Files after restart: %v", err)
	}
	if len(files) != 1 || files[0].Path != "/mnt/disk1/dir1/a.txt" {
		t.Fatalf("Files after restart = %+v, want the persisted entry with its path", files)
	}
}

// TestJournal_ReadLoop_DebouncesRapidBatchesButFlushesOnCleanStop is this
// issue's own reproduction and fix confirmation: readLoop once persisted
// a full snapshot after every single batch, so a burst of fanotify
// activity (extracting an archive) wrote once per batch instead of once
// per debounce window. The second batch here lands well within
// defaultJournalPersistInterval of the first persist, so it must be
// visible in Summary (in-memory state is never stale) but absent from
// the on-disk snapshot until a clean stop (RemoveDisk) flushes it.
func TestJournal_ReadLoop_DebouncesRapidBatchesButFlushesOnCleanStop(t *testing.T) {
	root := t.TempDir()
	w := NewFakeWatcher()
	j := NewJournal(w, root)
	ctx := context.Background()

	if err := j.AddDisk(ctx, "disk1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}

	snapPath := filepath.Join(root, "disk1.json")
	readSnapshot := func() journalSnapshot {
		t.Helper()
		data, err := os.ReadFile(snapPath)
		if err != nil {
			t.Fatalf("reading snapshot: %v", err)
		}
		var snap journalSnapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			t.Fatalf("parsing snapshot: %v", err)
		}
		return snap
	}

	// First batch ever for this disk: always persisted immediately, since
	// there is no earlier persist to debounce against.
	pushOne(w, "/mnt/disk1", ChangeEvent{Kind: ChangeCreate, ID: "dir1/a.txt", Name: "a.txt"})
	waitFor(t, time.Second, func() bool {
		_, err := os.ReadFile(snapPath)
		return err == nil
	})
	if got := len(readSnapshot().Entries); got != 1 {
		t.Fatalf("snapshot after the first batch has %d entries, want 1", got)
	}

	// Second batch, pushed immediately after: well inside the debounce
	// window, so it must not get its own write.
	pushOne(w, "/mnt/disk1", ChangeEvent{Kind: ChangeCreate, ID: "dir1/b.txt", Name: "b.txt"})
	waitFor(t, time.Second, func() bool {
		s, err := j.Summary("disk1")
		return err == nil && s.Count == 2
	})
	if got := len(readSnapshot().Entries); got != 1 {
		t.Fatalf("snapshot right after the second batch has %d entries, want 1 (still debounced, not yet written)", got)
	}

	// RemoveDisk (a clean stop) must flush the debounced batch before
	// returning — disarm already blocks on the read loop's own done
	// channel, so no extra synchronization is needed here.
	if err := j.RemoveDisk("disk1"); err != nil {
		t.Fatalf("RemoveDisk: %v", err)
	}
	if got := len(readSnapshot().Entries); got != 2 {
		t.Fatalf("snapshot after RemoveDisk has %d entries, want 2 (the debounced batch flushed on a clean stop)", got)
	}
}

func TestJournal_ResetSincePersists(t *testing.T) {
	root := t.TempDir()
	w := NewFakeWatcher()
	j := NewJournal(w, root)
	t.Cleanup(func() { _ = j.Close() })
	ctx := context.Background()

	if err := j.AddDisk(ctx, "disk1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}
	pushOne(w, "/mnt/disk1", ChangeEvent{Kind: ChangeCreate, ID: "dir1/a.txt", Name: "a.txt"})
	waitFor(t, time.Second, func() bool {
		s, err := j.Summary("disk1")
		return err == nil && s.Count == 1
	})

	if err := j.ResetSince("disk1"); err != nil {
		t.Fatalf("ResetSince: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "disk1.json"))
	if err != nil {
		t.Fatalf("reading persisted snapshot after ResetSince: %v", err)
	}
	var snap journalSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("parsing persisted snapshot: %v", err)
	}
	if len(snap.Entries) != 0 {
		t.Fatalf("persisted snapshot after ResetSince has %d entries, want 0", len(snap.Entries))
	}
}

func TestJournal_Close_StopsEveryDisk(t *testing.T) {
	w := NewFakeWatcher()
	j := NewJournal(w, "")
	ctx := context.Background()

	if err := j.AddDisk(ctx, "disk1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk disk1: %v", err)
	}
	if err := j.AddDisk(ctx, "disk2", "/mnt/disk2"); err != nil {
		t.Fatalf("AddDisk disk2: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := j.Summary("disk1"); err == nil {
		t.Fatal("Summary(disk1) after Close: got nil error")
	}
	if _, err := j.Summary("disk2"); err == nil {
		t.Fatal("Summary(disk2) after Close: got nil error")
	}
}
