package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/job"
)

func mapScheduleError(err error) error {
	switch {
	case errors.Is(err, ErrInvalidScheduleInput):
		return &apiError{code: "schedule_invalid_input", statusCode: 400, message: err.Error()}
	case errors.Is(err, ErrScheduleJobNotFound):
		return &apiError{code: "schedule_job_not_found", statusCode: 404, message: "no scheduled job with that id"}
	default:
		return err
	}
}

func (h *Handler) GetSchedules(ctx context.Context) (*apiv1.Schedules, error) {
	if h.Schedules == nil {
		return nil, fmt.Errorf("schedule service is not configured")
	}
	view, err := h.Schedules.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting schedules: %w", err)
	}
	return schedulesToAPI(ctx, view, h.Schedules), nil
}

func (h *Handler) UpdateMaintenanceChainSchedule(ctx context.Context, req *apiv1.UpdateMaintenanceChainScheduleRequest) (*apiv1.Schedules, error) {
	if h.Schedules == nil {
		return nil, fmt.Errorf("schedule service is not configured")
	}
	input := UpdateChainInput{}
	if req.StartTime.IsSet() {
		v := req.StartTime.Value
		input.StartTime = &v
	}
	if req.WeeklyScrubDay.IsSet() {
		v := int(req.WeeklyScrubDay.Value)
		input.WeeklyScrubDay = &v
	}
	if len(req.Steps) > 0 {
		input.StepEnabled = make(map[job.Step]bool, len(req.Steps))
		for _, step := range req.Steps {
			input.StepEnabled[job.Step(step.ID)] = step.Enabled
		}
	}
	view, err := h.Schedules.UpdateChain(ctx, input)
	if err != nil {
		return nil, mapScheduleError(err)
	}
	return schedulesToAPI(ctx, view, h.Schedules), nil
}

func (h *Handler) UpdateScheduledJob(ctx context.Context, req *apiv1.UpdateScheduledJobRequest, params apiv1.UpdateScheduledJobParams) (*apiv1.Schedules, error) {
	if h.Schedules == nil {
		return nil, fmt.Errorf("schedule service is not configured")
	}
	input := UpdateOtherJobInput{}
	if req.Enabled.IsSet() {
		v := req.Enabled.Value
		input.Enabled = &v
	}
	if req.Frequency.IsSet() {
		v := job.Frequency(req.Frequency.Value)
		input.Frequency = &v
	}
	if req.Time.IsSet() {
		v := req.Time.Value
		input.Time = &v
	}
	view, err := h.Schedules.UpdateOtherJob(ctx, string(params.JobId), input)
	if err != nil {
		return nil, mapScheduleError(err)
	}
	return schedulesToAPI(ctx, view, h.Schedules), nil
}

func schedulesToAPI(ctx context.Context, view SchedulesView, svc *ScheduleService) *apiv1.Schedules {
	now := time.Now()
	loc := time.UTC
	if svc != nil {
		if svc.Now != nil {
			now = svc.Now()
		}
		loc = svc.timezone(ctx)
	}
	chainNext := job.NextChainRun(now, loc, view.Chain.StartTime)
	steps := make([]apiv1.MaintenanceChainStep, 0, len(job.ChainOrder()))
	for _, step := range job.ChainOrder() {
		enabled := true
		if on, ok := view.Chain.Enabled[step]; ok {
			enabled = on
		}
		steps = append(steps, apiv1.MaintenanceChainStep{
			ID:      apiv1.MaintenanceChainStepId(step),
			Enabled: enabled,
		})
	}
	otherJobs := make([]apiv1.ScheduledJob, 0, len(view.OtherJobs))
	for _, j := range view.OtherJobs {
		otherJobs = append(otherJobs, apiv1.ScheduledJob{
			ID:              apiv1.OtherScheduleJobId(j.ID),
			Enabled:         j.Enabled,
			Frequency:       apiv1.ScheduleFrequency(j.Frequency),
			Time:            j.Time,
			SchedulePreview: job.OtherJobSchedulePreview(j.Frequency, j.Time),
			NextRun:         job.NextOtherJobRun(now, loc, j),
		})
	}
	conflicts := make([]apiv1.ScheduleConflict, 0, len(view.Conflicts))
	for _, c := range view.Conflicts {
		conflicts = append(conflicts, apiv1.ScheduleConflict{JobA: c.JobA, JobB: c.JobB})
	}
	return &apiv1.Schedules{
		Chain: apiv1.MaintenanceChainSchedule{
			StartTime:       view.Chain.StartTime,
			WeeklyScrubDay:  apiv1.Weekday(view.Chain.WeeklyScrubDay),
			SchedulePreview: job.ChainSchedulePreview(view.Chain.StartTime),
			NextRun:         chainNext,
			Steps:           steps,
		},
		OtherJobs: otherJobs,
		Conflicts: conflicts,
	}
}
