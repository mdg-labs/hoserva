package parity

import (
	"context"
	"testing"
	"time"
)

// TestFixPendingFiles_ReportsFilesWrittenSinceTheLastSync is this issue's
// own acceptance criterion: before a fix, the files the change journal
// recorded on the disk being fixed since the last successful sync — the
// ones reconstruction from parity can never restore (doc 02 §4).
func TestFixPendingFiles_ReportsFilesWrittenSinceTheLastSync(t *testing.T) {
	w := NewFakeWatcher()
	j := NewJournal(w, "")
	t.Cleanup(func() { _ = j.Close() })
	ctx := context.Background()

	if err := j.AddDisk(ctx, "d3", "/mnt/disk3"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}
	pushOne(w, "/mnt/disk3", ChangeEvent{Kind: ChangeCreate, ID: "movies/new.bin", Name: "new.bin"})
	waitFor(t, time.Second, func() bool {
		s, err := j.Summary("d3")
		return err == nil && s.Count == 1
	})

	lastSync := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	report, err := FixPendingFiles(j, "d3", lastSync)
	if err != nil {
		t.Fatalf("FixPendingFiles: %v", err)
	}
	if report.Disk != "d3" {
		t.Errorf("Disk = %q, want d3", report.Disk)
	}
	if !report.LastSyncAt.Equal(lastSync) {
		t.Errorf("LastSyncAt = %v, want %v", report.LastSyncAt, lastSync)
	}
	if report.Count != 1 {
		t.Errorf("Count = %d, want 1", report.Count)
	}
	if len(report.Files) != 1 || report.Files[0].Name != "new.bin" {
		t.Errorf("Files = %+v, want one entry named new.bin", report.Files)
	}
	if report.Incomplete {
		t.Error("Incomplete = true, want false — no overflow was scripted")
	}
}

// TestFixPendingFiles_IncompleteWhenTheJournalOverflowed is Q13's own
// rule: an overflowed journal must say its list may be missing entries,
// never present a partial count as if it were exact.
func TestFixPendingFiles_IncompleteWhenTheJournalOverflowed(t *testing.T) {
	w := NewFakeWatcher()
	j := NewJournal(w, "")
	t.Cleanup(func() { _ = j.Close() })
	ctx := context.Background()

	if err := j.AddDisk(ctx, "d3", "/mnt/disk3"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}
	w.Stream("/mnt/disk3").Push([]StreamEvent{{Overflow: true}})
	waitFor(t, time.Second, func() bool {
		s, err := j.Summary("d3")
		return err == nil && s.Overflowed
	})

	report, err := FixPendingFiles(j, "d3", time.Now())
	if err != nil {
		t.Fatalf("FixPendingFiles: %v", err)
	}
	if !report.Incomplete {
		t.Error("Incomplete = false, want true — the journal overflowed since the last sync")
	}
}

func TestFixPendingFiles_UnknownDiskErrors(t *testing.T) {
	j := NewJournal(NewFakeWatcher(), "")
	t.Cleanup(func() { _ = j.Close() })
	if _, err := FixPendingFiles(j, "nope", time.Now()); err == nil {
		t.Fatal("FixPendingFiles: got nil error for an untracked disk")
	}
}
