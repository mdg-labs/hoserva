package api_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/job"
)

// TestHandler_StartMover_OnBattery_Returns409OnBattery proves #356: the
// scheduler's ErrOnBattery (Q77's mover/sync hold, doc 02 §6) reaches the
// API as a distinguishable 409 `on_battery`, not the generic 500
// `internal` NewError's default falls back to for any error that isn't
// classified into an *apiError.
func TestHandler_StartMover_OnBattery_Returns409OnBattery(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	r.Register(job.TypeMover, true, func(context.Context, *job.RunContext) error { return nil })

	s.PauseForBattery(ctx)

	_, err := h.StartMover(ctx)
	status := apiError(t, h, err)
	if status.StatusCode != 409 {
		t.Fatalf("StartMover on battery: status = %d, want 409", status.StatusCode)
	}
	if status.Response.Code != "on_battery" {
		t.Fatalf("StartMover on battery: code = %q, want on_battery", status.Response.Code)
	}
}

// TestHandler_StartSync_OnBattery_Returns409OnBattery is StartMover's
// sibling for the sync side of Q77's hold.
func TestHandler_StartSync_OnBattery_Returns409OnBattery(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	r.Register(job.TypeSync, false, func(context.Context, *job.RunContext) error { return nil })

	s.PauseForBattery(ctx)

	_, err := h.StartSync(ctx, &apiv1.StartSyncRequest{})
	status := apiError(t, h, err)
	if status.StatusCode != 409 {
		t.Fatalf("StartSync on battery: status = %d, want 409", status.StatusCode)
	}
	if status.Response.Code != "on_battery" {
		t.Fatalf("StartSync on battery: code = %q, want on_battery", status.Response.Code)
	}
}

// TestHandler_ResumeJob_OnBattery_Returns409OnBattery proves the same
// mapping for Resume's own ErrOnBattery refusal (job.Scheduler.Resume) of
// an interrupted mover job while the on-battery hold is active — reached
// through maintenance mode's own graceful-stop, kept separate from
// PauseForBattery's own resume round trip, the same way
// TestScheduler_Resume_RefusesBatteryHeldTypeWhileOnBattery isolates it.
func TestHandler_ResumeJob_OnBattery_Returns409OnBattery(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	stopSeen := make(chan struct{})
	r.Register(job.TypeMover, true, func(ctx context.Context, rc *job.RunContext) error {
		<-rc.StopRequested()
		close(stopSeen)
		return rc.SaveCheckpoint([]byte("checkpoint"))
	})

	j, err := s.Submit(ctx, job.TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	<-stopSeen
	waitForStatus(t, h.Store, j.ID, job.StatusInterrupted)
	s.ExitMaintenance()
	s.PauseForBattery(ctx)

	id, parseErr := uuid.Parse(j.ID)
	if parseErr != nil {
		t.Fatalf("uuid.Parse: %v", parseErr)
	}
	_, err = h.ResumeJob(ctx, apiv1.ResumeJobParams{JobId: id})
	status := apiError(t, h, err)
	if status.StatusCode != 409 {
		t.Fatalf("Resume on battery: status = %d, want 409", status.StatusCode)
	}
	if status.Response.Code != "on_battery" {
		t.Fatalf("Resume on battery: code = %q, want on_battery", status.Response.Code)
	}
}
