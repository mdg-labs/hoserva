package parity

import (
	"errors"
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

// TestParseRunSummary_NoOpAfterReplace is a real, unedited `-l` log
// captured on a real Debian 13 guest with real disk UUIDs: disk1 replaced
// (wiped, reformatted with a genuinely new filesystem UUID, and rebuilt
// with a real `snapraid fix -d d1`), then a real follow-up sync finding
// nothing left to do — printing the exact `WARNING! UUID is changed for
// disks: 'd1'` line a real-world report of a no-op sync being
// misclassified as a failed job quoted (tracked separately from this
// dry-run fix). It carries a full `summary:` section (both the pre-sync
// diff's own summary:exit:equal and a terminal summary:exit:ok) and
// already parses as success today — kept as a regression test so this
// real, exact scenario stays covered.
func TestParseRunSummary_NoOpAfterReplace(t *testing.T) {
	s, err := ParseRunSummary(readCorpus(t, "snapraid_sync_noop_after_replace.log"))
	if err != nil {
		t.Fatalf("ParseRunSummary: %v", err)
	}
	if s.Exit != "ok" {
		t.Fatalf("Exit = %q, want %q", s.Exit, "ok")
	}
}

// TestParseRunSummary_TruncatedStillFails is a real `-l` log from a
// snapraid sync process SIGKILLed 20ms after it started: it has the
// header tags (version, command, argv, selftest) and one msg:progress
// line, but stops there — no summary tag of any kind. Kept as a
// regression test: a truncated run is still reported as the parse
// failure it is.
func TestParseRunSummary_TruncatedStillFails(t *testing.T) {
	s, err := ParseRunSummary(readCorpus(t, "snapraid_sync_truncated_no_summary.log"))
	if err == nil {
		t.Fatalf("ParseRunSummary: got %+v, nil error; want a failure for a truncated log", s)
	}
	if !errors.Is(err, ErrRunParse) {
		t.Fatalf("error = %v, want it to wrap ErrRunParse", err)
	}
}

// corruptedContentSyncLog is a real, unedited `-l` log's own tail,
// captured running `snapraid sync` in the loop-device lab (#390) against
// a content file truncated to simulate what an unclean shutdown can leave
// behind: a real snapraid 12.4-1 binary exits non-zero in well under a
// second, prints these two msg:fatal lines, and never reaches — let alone
// writes — a summary tag of any kind.
const corruptedContentSyncLog = `msg:progress: Loading state from /lab/390-a1/mnt/parity1/snapraid.content...
msg:fatal: Unexpected end of content file '/lab/390-a1/mnt/parity1/snapraid.content' at offset 2000
msg:fatal: This content file is truncated. Please use an alternate copy.
`

// TestFatalMessages_ExtractsSnapraidsOwnDiagnostic is #390's own coverage
// for the helper runFailedBeforeSummaryErr relies on: a run that never
// wrote a summary section still has SnapRAID's own fatal text sitting
// right there in the log, and it must come back in order, not be lost the
// way the plain ErrRunParse case discarded it.
func TestFatalMessages_ExtractsSnapraidsOwnDiagnostic(t *testing.T) {
	got := fatalMessages([]byte(corruptedContentSyncLog))
	want := []string{
		"Unexpected end of content file '/lab/390-a1/mnt/parity1/snapraid.content' at offset 2000",
		"This content file is truncated. Please use an alternate copy.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fatalMessages = %#v, want %#v", got, want)
	}
}

// TestFatalMessages_NoneFoundReturnsNil covers the plain truncated-mid-run
// case (TestParseRunSummary_TruncatedStillFails's own fixture): a process
// SIGKILLed before it ever got the chance to print anything fatal has no
// msg:fatal lines to find, and fatalMessages must say so by returning
// nothing rather than inventing one.
func TestFatalMessages_NoneFoundReturnsNil(t *testing.T) {
	got := fatalMessages(readCorpus(t, "snapraid_sync_truncated_no_summary.log"))
	if got != nil {
		t.Fatalf("fatalMessages = %#v, want nil", got)
	}
}

// TestParseRunSummary_FixUnrecoverableFileIsListed covers a real fix log
// (fixUnrecoverableLog) in which SnapRAID could not rebuild a file: the
// `status:unrecoverable:<disk>:<path>` line is collected beside the
// unrecoverable count, and the file is not counted as recovered.
func TestParseRunSummary_FixUnrecoverableFileIsListed(t *testing.T) {
	s, err := ParseRunSummary([]byte(fixUnrecoverableLog))
	if err != nil {
		t.Fatalf("ParseRunSummary: %v", err)
	}
	if want := []string{"d3:pr/unrec.bin"}; !reflect.DeepEqual(s.UnrecoveredFiles, want) {
		t.Fatalf("UnrecoveredFiles = %#v, want %#v", s.UnrecoveredFiles, want)
	}
	if s.Unrecoverable != 1 || len(s.RecoveredFiles) != 0 || s.Exit != "unrecoverable" {
		t.Fatalf("summary = %+v, want 1 unrecoverable, no recovered file, exit unrecoverable", s)
	}
}
