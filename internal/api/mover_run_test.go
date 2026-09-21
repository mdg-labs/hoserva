package api_test

import (
	"context"
	"testing"

	"github.com/mdg-labs/hoserva/internal/job"
)

// TestHandler_StartMover_SubmitsTypeMover proves the manual trigger
// (`hoserva mover run`, doc 09 §2) submits the exact same TypeMover job
// the threshold poll and the nightly chain do — no second
// mover-invocation path (#235 AC4).
func TestHandler_StartMover_SubmitsTypeMover(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	r.Register(job.TypeMover, true, func(context.Context, *job.RunContext) error { return nil })

	got, err := h.StartMover(ctx)
	if err != nil {
		t.Fatalf("StartMover: %v", err)
	}
	if got.Type != "mover" {
		t.Fatalf("Type = %q, want mover", got.Type)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s, want succeeded", finished.Status)
	}
}

// TestHandler_StartMover_NoSchedulerIsAnError proves an unconfigured
// scheduler is reported rather than a nil-pointer panic — the same
// defensive check startSync/startScrub/startFix already make.
func TestHandler_StartMover_NoSchedulerIsAnError(t *testing.T) {
	h, _, _ := newTestHandler(t)
	h.Scheduler = nil

	if _, err := h.StartMover(context.Background()); err == nil {
		t.Fatal("expected an error with no scheduler configured")
	}
}
