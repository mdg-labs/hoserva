package api

import (
	"fmt"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/job"
)

// jobToAPI translates internal/job's own domain type into the generated
// apiv1.Job the spec declares (D18) — the only shape this package ever
// hands back to a caller.
func jobToAPI(j *job.Job) (*apiv1.Job, error) {
	id, err := uuid.Parse(j.ID)
	if err != nil {
		return nil, fmt.Errorf("job %s has a non-UUID id: %w", j.ID, err)
	}

	out := &apiv1.Job{
		ID:          id,
		Type:        apiv1.JobType(j.Type),
		Class:       apiv1.JobClass(j.Class),
		Status:      apiv1.JobStatus(j.Status),
		Resumable:   j.Resumable,
		Cancellable: j.Cancellable,
		CreatedAt:   j.CreatedAt,
	}
	if j.Progress != nil {
		out.Progress = apiv1.NewOptNilInt32(int32(*j.Progress))
	}
	if j.StartedAt != nil {
		out.StartedAt = apiv1.NewOptNilDateTime(*j.StartedAt)
	}
	if j.FinishedAt != nil {
		out.FinishedAt = apiv1.NewOptNilDateTime(*j.FinishedAt)
	}
	if j.ErrorCode != "" || j.ErrorMessage != "" {
		out.Error = apiv1.NewOptNilError(apiv1.Error{Code: j.ErrorCode, Message: j.ErrorMessage})
	}
	return out, nil
}
