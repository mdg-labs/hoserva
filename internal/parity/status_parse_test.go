package parity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// corpusDir holds real output SnapRAID actually printed, captured with a
// real snapraid 12.4-1 binary against real loop disks in the lab
// (doc 06 §2, doc 06 §3) — never hand-authored.
const corpusDir = "../../testdata/parsers"

func readCorpus(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(corpusDir, name))
	if err != nil {
		t.Fatalf("reading corpus fixture %s: %v", name, err)
	}
	return data
}

func TestParseStatus_NewArray(t *testing.T) {
	r, err := ParseStatus(readCorpus(t, "snapraid_status_new_array.log"))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	wantMounts := map[string]string{
		"d1": "/lab/28-a1/mnt/disk1/",
		"d2": "/lab/28-a1/mnt/disk2/",
		"d3": "/lab/28-a1/mnt/disk3/",
	}
	for id, path := range wantMounts {
		if r.DataMounts[id] != path {
			t.Errorf("DataMounts[%s] = %q, want %q", id, r.DataMounts[id], path)
		}
	}
	wantCounts := map[string]int{"d1": 1, "d2": 1, "d3": 1}
	for id, n := range wantCounts {
		if r.PerDiskFileCount[id] != n {
			t.Errorf("PerDiskFileCount[%s] = %d, want %d", id, r.PerDiskFileCount[id], n)
		}
	}
	if r.ZeroSubsecondFiles != 0 {
		t.Errorf("ZeroSubsecondFiles = %d, want 0", r.ZeroSubsecondFiles)
	}
	if r.ChangedSinceSync != 0 {
		t.Errorf("ChangedSinceSync = %d, want 0", r.ChangedSinceSync)
	}
	if r.Unscrubbed != 2 {
		t.Errorf("Unscrubbed = %d, want 2", r.Unscrubbed)
	}
	if r.BadBlocks != 0 {
		t.Errorf("BadBlocks = %d, want 0", r.BadBlocks)
	}
	if r.ParityDisks != 1 {
		t.Errorf("ParityDisks = %d, want 1", r.ParityDisks)
	}
	if want := time.Unix(1789712928, 0); !r.LastActivityAt.Equal(want) {
		t.Errorf("LastActivityAt = %v, want %v", r.LastActivityAt, want)
	}
}

// TestParseStatus_ZeroSubsecond is Q17's own real signal: SnapRAID's
// structured log reports `summary:zerosubsecond_file_count:1` for the
// exact same array state whose human output says "You have 1 files with
// a zero sub-second timestamp." — captured from the same real run.
func TestParseStatus_ZeroSubsecond(t *testing.T) {
	r, err := ParseStatus(readCorpus(t, "snapraid_status_zerosubsecond.log"))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if r.ZeroSubsecondFiles != 1 {
		t.Fatalf("ZeroSubsecondFiles = %d, want 1", r.ZeroSubsecondFiles)
	}
}

func TestParseStatus_AfterTouch_ClearsZeroSubsecond(t *testing.T) {
	r, err := ParseStatus(readCorpus(t, "snapraid_status_after_touch.log"))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if r.ZeroSubsecondFiles != 0 {
		t.Fatalf("ZeroSubsecondFiles = %d, want 0 after touch", r.ZeroSubsecondFiles)
	}
}

func TestParseStatus_BadBlocks(t *testing.T) {
	r, err := ParseStatus(readCorpus(t, "snapraid_status_bad_blocks.log"))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if r.BadBlocks != 3 {
		t.Fatalf("BadBlocks = %d, want 3", r.BadBlocks)
	}
	if got := r.ToParityStatus().Freshness; got != FreshnessRed {
		t.Fatalf("ToParityStatus().Freshness = %v, want FreshnessRed", got)
	}
}

func TestParseStatus_Clean(t *testing.T) {
	r, err := ParseStatus(readCorpus(t, "snapraid_status_clean.log"))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if r.BadBlocks != 0 || r.ChangedSinceSync != 0 {
		t.Fatalf("expected a fully clean status, got BadBlocks=%d ChangedSinceSync=%d", r.BadBlocks, r.ChangedSinceSync)
	}
	if got := r.ToParityStatus().Freshness; got != FreshnessGreen {
		t.Fatalf("ToParityStatus().Freshness = %v, want FreshnessGreen", got)
	}
}

func TestParseStatus_EmptyInput(t *testing.T) {
	if _, err := ParseStatus(nil); err == nil {
		t.Fatal("ParseStatus(nil): got nil error")
	}
}

func TestParseStatus_Malformed(t *testing.T) {
	if _, err := ParseStatus([]byte("not a snapraid log\nat all\n")); err == nil {
		t.Fatal("ParseStatus(malformed): got nil error, want ErrStatusParse")
	}
}
