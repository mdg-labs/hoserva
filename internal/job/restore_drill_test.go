package job

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRestoreDrillJob_IsServiceClassNotResumableAndTakesNoParams(t *testing.T) {
	class, ok := ClassOf(TypeRestoreDrill)
	if !ok || class != ClassService {
		t.Fatalf("ClassOf = %q, %v; want the service class", class, ok)
	}
	if Resumable(TypeRestoreDrill) {
		t.Fatal("a restore drill is re-run, never resumed")
	}
	if err := ValidateParams(TypeRestoreDrill, nil); err != nil {
		t.Fatalf("no params: %v", err)
	}
	if err := ValidateParams(TypeRestoreDrill, []byte(`{"destination":"x"}`)); err == nil {
		t.Fatal("a restore drill accepted params")
	}
}

func TestRunRestoreDrill_APassedDrillSucceedsAndALogLineSaysSo(t *testing.T) {
	s := newTestScheduler(t)
	s.registry.Register(TypeRestoreDrill, true, RunRestoreDrill(func(_ context.Context, out io.Writer) error {
		_, _ = io.WriteString(out, "Local: verified an archive\n")
		return nil
	}))
	j, err := s.Submit(context.Background(), TypeRestoreDrill, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if done := await(t, s, j.ID); done.Status != StatusSucceeded {
		t.Fatalf("status = %s %q, want succeeded", done.Status, done.ErrorMessage)
	}
}

func TestRunRestoreDrill_AFailedDrillFailsTheJobWithItsReason(t *testing.T) {
	s := newTestScheduler(t)
	s.registry.Register(TypeRestoreDrill, true, RunRestoreDrill(func(context.Context, io.Writer) error {
		return errors.New("restore drill failed: Local: checksum mismatch for state.db")
	}))
	j, err := s.Submit(context.Background(), TypeRestoreDrill, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := await(t, s, j.ID)
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "checksum mismatch") {
		t.Fatalf("job = %s %q, want failed with the drill's reason", done.Status, done.ErrorMessage)
	}
}
