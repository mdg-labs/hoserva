package parity

import (
	"reflect"
	"testing"
)

func TestParseRunSummary_SyncOK(t *testing.T) {
	s, err := ParseRunSummary(readCorpus(t, "snapraid_sync_ok.log"))
	if err != nil {
		t.Fatalf("ParseRunSummary: %v", err)
	}
	// The log also carries sync's own internal pre-sync diff, whose
	// summary:exit:diff line comes first — the sync's own final
	// summary:exit:ok, which comes last, is what must win.
	if s.Exit != "ok" {
		t.Fatalf("Exit = %q, want %q", s.Exit, "ok")
	}
	if s.FileErrors != 0 || s.IOErrors != 0 || s.DataErrors != 0 {
		t.Fatalf("expected a clean sync, got %+v", s)
	}
}

func TestParseRunSummary_ScrubDataErrors(t *testing.T) {
	s, err := ParseRunSummary(readCorpus(t, "snapraid_scrub_data_errors.log"))
	if err != nil {
		t.Fatalf("ParseRunSummary: %v", err)
	}
	if s.Exit != "error" {
		t.Fatalf("Exit = %q, want %q", s.Exit, "error")
	}
	if s.DataErrors != 3 {
		t.Fatalf("DataErrors = %d, want 3", s.DataErrors)
	}
}

func TestParseRunSummary_ScrubClearsBad(t *testing.T) {
	s, err := ParseRunSummary(readCorpus(t, "snapraid_scrub_clears_bad.log"))
	if err != nil {
		t.Fatalf("ParseRunSummary: %v", err)
	}
	if s.Exit != "ok" || s.DataErrors != 0 {
		t.Fatalf("expected a clean rescrub, got %+v", s)
	}
}

// TestParseRunSummary_FixUndelete is the fix-restores-a-deleted-file
// case: one recovered file, its own array-relative path named in
// RecoveredFiles.
func TestParseRunSummary_FixUndelete(t *testing.T) {
	s, err := ParseRunSummary(readCorpus(t, "snapraid_fix_undelete.log"))
	if err != nil {
		t.Fatalf("ParseRunSummary: %v", err)
	}
	if s.Exit != "recovered" {
		t.Fatalf("Exit = %q, want %q", s.Exit, "recovered")
	}
	if s.Errors != 1 || s.Recovered != 1 || s.Unrecoverable != 0 {
		t.Fatalf("expected 1 error, 1 recovered, 0 unrecoverable, got %+v", s)
	}
	want := []string{"d2:docs/b.bin"}
	if !reflect.DeepEqual(s.RecoveredFiles, want) {
		t.Fatalf("RecoveredFiles = %v, want %v", s.RecoveredFiles, want)
	}
}

func TestParseRunSummary_FixRecoversCorruption(t *testing.T) {
	s, err := ParseRunSummary(readCorpus(t, "snapraid_fix_recovers_corruption.log"))
	if err != nil {
		t.Fatalf("ParseRunSummary: %v", err)
	}
	if s.Errors != 3 || s.Recovered != 3 || s.Unrecoverable != 0 {
		t.Fatalf("expected 3 errors, 3 recovered, 0 unrecoverable, got %+v", s)
	}
}

func TestParseRunSummary_CheckOK(t *testing.T) {
	s, err := ParseRunSummary(readCorpus(t, "snapraid_check_ok.log"))
	if err != nil {
		t.Fatalf("ParseRunSummary: %v", err)
	}
	if s.Exit != "ok" || s.Errors != 0 {
		t.Fatalf("expected a clean check, got %+v", s)
	}
}

func TestParseRunSummary_CheckAuditOnly(t *testing.T) {
	s, err := ParseRunSummary(readCorpus(t, "snapraid_check_audit_only.log"))
	if err != nil {
		t.Fatalf("ParseRunSummary: %v", err)
	}
	if s.Exit != "ok" {
		t.Fatalf("Exit = %q, want %q", s.Exit, "ok")
	}
}

func TestParseRunSummary_EmptyInput(t *testing.T) {
	if _, err := ParseRunSummary(nil); err == nil {
		t.Fatal("ParseRunSummary(nil): got nil error")
	}
}
