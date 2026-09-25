package main

import (
	"context"
	"fmt"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/job"
)

func errScheduleInvalidInput(msg string) error {
	return &mockError{code: "schedule_invalid_input", statusCode: 400, message: msg}
}

// mockValidateClock and mockValidFrequency mirror internal/api's own
// unexported validateClock/validFrequency (scheduleservice.go): a
// schedule input the daemon rejects must be rejected here too, not
// silently accepted.
func mockValidateClock(value string) error {
	if _, _, err := job.ParseClock(value); err != nil {
		return errScheduleInvalidInput(err.Error())
	}
	return nil
}

func mockValidFrequency(freq apiv1.ScheduleFrequency) bool {
	switch job.Frequency(freq) {
	case job.FrequencyDaily, job.FrequencyWeekly, job.FrequencyMonthly:
		return true
	default:
		return false
	}
}

func (h *handler) GetSchedules(ctx context.Context) (*apiv1.Schedules, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	return cloneSchedules(h.schedules), nil
}

func (h *handler) UpdateMaintenanceChainSchedule(ctx context.Context, req *apiv1.UpdateMaintenanceChainScheduleRequest) (*apiv1.Schedules, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	start, hasStart := req.StartTime.Get()
	if hasStart {
		if err := mockValidateClock(start); err != nil {
			return nil, err
		}
	}
	day, hasDay := req.WeeklyScrubDay.Get()
	if hasDay && (day < 0 || day > 6) {
		return nil, errScheduleInvalidInput("weeklyScrubDay must be 0-6")
	}
	if hasStart {
		h.schedules.Chain.StartTime = start
	}
	if hasDay {
		h.schedules.Chain.WeeklyScrubDay = day
	}
	for _, step := range req.Steps {
		for i := range h.schedules.Chain.Steps {
			if h.schedules.Chain.Steps[i].ID == step.ID {
				h.schedules.Chain.Steps[i].Enabled = step.Enabled
				break
			}
		}
	}
	recomputeMockSchedules(&h.schedules)
	return cloneSchedules(h.schedules), nil
}

func (h *handler) UpdateScheduledJob(ctx context.Context, req *apiv1.UpdateScheduledJobRequest, params apiv1.UpdateScheduledJobParams) (*apiv1.Schedules, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	for i := range h.schedules.OtherJobs {
		if h.schedules.OtherJobs[i].ID != params.JobId {
			continue
		}
		j := h.schedules.OtherJobs[i]
		if enabled, ok := req.Enabled.Get(); ok {
			j.Enabled = enabled
		}
		if freq, ok := req.Frequency.Get(); ok {
			if !mockValidFrequency(freq) {
				return nil, errScheduleInvalidInput(fmt.Sprintf("unknown frequency %q", freq))
			}
			j.Frequency = freq
		}
		if t, ok := req.Time.Get(); ok {
			if err := mockValidateClock(t); err != nil {
				return nil, err
			}
			j.Time = t
		}
		h.schedules.OtherJobs[i] = j
		recomputeMockSchedules(&h.schedules)
		return cloneSchedules(h.schedules), nil
	}
	return nil, &mockError{code: "schedule_job_not_found", statusCode: 404, message: "no scheduled job with that id"}
}

func defaultMockSchedules() apiv1.Schedules {
	s := apiv1.Schedules{
		Chain: apiv1.MaintenanceChainSchedule{
			StartTime:      job.DefaultChainStartTime,
			WeeklyScrubDay: job.DefaultWeeklyScrubDay,
			Steps:          defaultMockChainSteps(),
		},
		OtherJobs: defaultMockOtherJobs(),
	}
	recomputeMockSchedules(&s)
	return s
}

func defaultMockChainSteps() []apiv1.MaintenanceChainStep {
	chain := job.DefaultChainSettings()
	steps := make([]apiv1.MaintenanceChainStep, 0, len(job.ChainOrder()))
	for _, step := range job.ChainOrder() {
		enabled := true
		if on, ok := chain.Enabled[step]; ok {
			enabled = on
		}
		steps = append(steps, apiv1.MaintenanceChainStep{
			ID:      apiv1.MaintenanceChainStepId(step),
			Enabled: enabled,
		})
	}
	return steps
}

func defaultMockOtherJobs() []apiv1.ScheduledJob {
	out := make([]apiv1.ScheduledJob, 0, len(job.DefaultOtherJobs()))
	for _, j := range job.DefaultOtherJobs() {
		out = append(out, apiv1.ScheduledJob{
			ID:        apiv1.OtherScheduleJobId(j.ID),
			Enabled:   j.Enabled,
			Frequency: apiv1.ScheduleFrequency(j.Frequency),
			Time:      j.Time,
		})
	}
	return out
}

func recomputeMockSchedules(s *apiv1.Schedules) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	loc := time.UTC
	chain := job.DefaultChainSettings()
	chain.StartTime = s.Chain.StartTime
	chain.WeeklyScrubDay = int(s.Chain.WeeklyScrubDay)
	chain.Enabled = map[job.Step]bool{}
	for _, step := range s.Chain.Steps {
		chain.Enabled[job.Step(step.ID)] = step.Enabled
	}
	s.Chain.SchedulePreview = job.ChainSchedulePreview(chain.StartTime)
	s.Chain.NextRun = job.NextChainRun(now, loc, chain.StartTime)

	others := make([]job.OtherJobSettings, 0, len(s.OtherJobs))
	for i := range s.OtherJobs {
		j := job.OtherJobSettings{
			ID:        string(s.OtherJobs[i].ID),
			Enabled:   s.OtherJobs[i].Enabled,
			Frequency: job.Frequency(s.OtherJobs[i].Frequency),
			Time:      s.OtherJobs[i].Time,
		}
		others = append(others, j)
		s.OtherJobs[i].SchedulePreview = job.OtherJobSchedulePreview(j.Frequency, j.Time)
		s.OtherJobs[i].NextRun = job.NextOtherJobRun(now, loc, j)
	}
	conflicts := job.DetectScheduleConflicts(job.BuildScheduleWindows(now, loc, chain, others))
	s.Conflicts = make([]apiv1.ScheduleConflict, 0, len(conflicts))
	for _, c := range conflicts {
		s.Conflicts = append(s.Conflicts, apiv1.ScheduleConflict{JobA: c.JobA, JobB: c.JobB})
	}
}

func cloneSchedules(in apiv1.Schedules) *apiv1.Schedules {
	out := in
	out.Chain.Steps = append([]apiv1.MaintenanceChainStep(nil), in.Chain.Steps...)
	out.OtherJobs = append([]apiv1.ScheduledJob(nil), in.OtherJobs...)
	out.Conflicts = append([]apiv1.ScheduleConflict(nil), in.Conflicts...)
	return &out
}
