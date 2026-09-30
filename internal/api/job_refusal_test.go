package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mdg-labs/hoserva/internal/job"
)

func TestMapSchedulerError_ResumeAndAbortInProgressAreDistinct409s(t *testing.T) {
	id := uuid.New()
	tests := []struct {
		name        string
		err         error
		wantCode    string
		wantMessage string
	}{
		{"resume repairing its log", job.ErrJobResumeInProgress, "job_resume_in_progress", fmt.Sprintf("job %s is being resumed right now — try again in a moment", id)},
		{"resume repairing its log, wrapped", fmt.Errorf("cancel: %w", job.ErrJobResumeInProgress), "job_resume_in_progress", fmt.Sprintf("job %s is being resumed right now — try again in a moment", id)},
		{"cancel unwinding", job.ErrJobAbortInProgress, "job_abort_in_progress", fmt.Sprintf("job %s is being aborted right now", id)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := (&Handler{}).NewError(context.Background(), mapSchedulerError(id, tt.err))
			if status.StatusCode != 409 || status.Response.Code != tt.wantCode || status.Response.Message != tt.wantMessage {
				t.Fatalf("mapSchedulerError(%v) = %d %s %q, want 409 %s %q", tt.err, status.StatusCode, status.Response.Code, status.Response.Message, tt.wantCode, tt.wantMessage)
			}
			if tt.wantCode == "job_resume_in_progress" && strings.Contains(status.Response.Message, "abort") {
				t.Fatalf("resume refusal message mentions an abort: %q", status.Response.Message)
			}
		})
	}
}
