package parity

import (
	"context"
	"testing"
	"time"
)

func TestRelocationManifestStore_EmptyBeforeAnyReplace(t *testing.T) {
	ctx := context.Background()
	s := NewRelocationManifestStore(newTestDB(t))

	manifest, removingDisks, err := s.Current(ctx)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if len(manifest) != 0 {
		t.Fatalf("manifest = %+v, want empty before any Replace", manifest)
	}
	if len(removingDisks) != 0 {
		t.Fatalf("removingDisks = %+v, want empty before any Replace", removingDisks)
	}
}

func TestRelocationManifestStore_ReplaceAndCurrent(t *testing.T) {
	ctx := context.Background()
	s := NewRelocationManifestStore(newTestDB(t))
	mtime := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	manifest := []ManifestEntry{
		{RelPath: "movies/a.mkv", Size: 100, MTime: mtime, SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
		{RelPath: "movies/b.mkv", Size: 200, MTime: mtime, SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
	}
	removingDisks := map[string]bool{"/mnt/disk3": true}

	if err := s.Replace(ctx, manifest, removingDisks); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	gotManifest, gotRemoving, err := s.Current(ctx)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if len(gotManifest) != 2 {
		t.Fatalf("manifest = %+v, want 2 entries", gotManifest)
	}
	if gotManifest[0].RelPath != "movies/a.mkv" || gotManifest[0].SourceDisk != "/mnt/disk1" || gotManifest[0].TargetDisk != "/mnt/disk2" || gotManifest[0].Size != 100 {
		t.Errorf("manifest[0] = %+v, want the first replaced entry", gotManifest[0])
	}
	if !gotManifest[0].MTime.Equal(mtime) {
		t.Errorf("manifest[0].MTime = %v, want %v", gotManifest[0].MTime, mtime)
	}
	if !gotRemoving["/mnt/disk3"] {
		t.Fatalf("removingDisks = %+v, want /mnt/disk3", gotRemoving)
	}
	if len(gotRemoving) != 1 {
		t.Fatalf("removingDisks = %+v, want exactly one entry", gotRemoving)
	}
}

// TestRelocationManifestStore_ReplaceClearsPreviousRows confirms a second
// Replace fully supersedes the first — including clearing back to empty
// when a relocation job's own trailing sync has finished (Replace(ctx,
// nil, nil)), the state that means "no relocation in progress" and must
// match Guard.Evaluate's own pre-#194 nil-manifest behaviour exactly.
func TestRelocationManifestStore_ReplaceClearsPreviousRows(t *testing.T) {
	ctx := context.Background()
	s := NewRelocationManifestStore(newTestDB(t))
	mtime := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	first := []ManifestEntry{{RelPath: "a", Size: 1, MTime: mtime, SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"}}
	if err := s.Replace(ctx, first, map[string]bool{"/mnt/disk3": true}); err != nil {
		t.Fatalf("Replace (first): %v", err)
	}

	if err := s.Replace(ctx, nil, nil); err != nil {
		t.Fatalf("Replace (clear): %v", err)
	}

	manifest, removingDisks, err := s.Current(ctx)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if len(manifest) != 0 {
		t.Fatalf("manifest = %+v, want empty after a clearing Replace", manifest)
	}
	if len(removingDisks) != 0 {
		t.Fatalf("removingDisks = %+v, want empty after a clearing Replace", removingDisks)
	}
}
