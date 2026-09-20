package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mdg-labs/hoserva/internal/job"
)

// ErrInvalidScheduleInput is returned when schedule input fails validation.
var ErrInvalidScheduleInput = errors.New("schedule: invalid input")

// ScheduleService owns schedule persistence, next-run computation and
// conflict detection (#197, doc 03 §8.4, Q30).
type ScheduleService struct {
	Schedules *ScheduleStore
	Settings  *SettingsStore
	Now       func() time.Time
	mu        sync.Mutex
}

// NewScheduleService wires a ScheduleService with the real clock.
func NewScheduleService(schedules *ScheduleStore, settings *SettingsStore) *ScheduleService {
	return &ScheduleService{
		Schedules: schedules,
		Settings:  settings,
		Now:       time.Now,
	}
}

func (s *ScheduleService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// SchedulesView is the API-facing schedules response.
type SchedulesView struct {
	Chain     job.ChainSettings
	OtherJobs []job.OtherJobSettings
	Conflicts []job.ScheduleConflict
}

// Get returns the current schedules with computed next runs and conflicts.
func (s *ScheduleService) Get(ctx context.Context) (SchedulesView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(ctx)
}

func (s *ScheduleService) getLocked(ctx context.Context) (SchedulesView, error) {
	if err := s.Schedules.EnsureDefaults(ctx, s.now().UTC().Format(timeFormat)); err != nil {
		return SchedulesView{}, err
	}
	chain, err := s.loadChain(ctx)
	if err != nil {
		return SchedulesView{}, err
	}
	others, err := s.loadOtherJobs(ctx)
	if err != nil {
		return SchedulesView{}, err
	}
	loc := s.timezone(ctx)
	now := s.now()
	conflicts := job.DetectScheduleConflicts(job.BuildScheduleWindows(now, loc, chain, others))
	return SchedulesView{Chain: chain, OtherJobs: others, Conflicts: conflicts}, nil
}

// ChainEnabled returns the persisted enabled map for MaintenanceChain.Run.
func (s *ScheduleService) ChainEnabled(ctx context.Context) (map[job.Step]bool, error) {
	view, err := s.Get(ctx)
	if err != nil {
		return nil, err
	}
	return view.Chain.Enabled, nil
}

// UpdateChainInput carries optional chain fields from an update request.
type UpdateChainInput struct {
	StartTime      *string
	WeeklyScrubDay *int
	StepEnabled    map[job.Step]bool
}

// UpdateChain persists chain settings and returns the updated view.
func (s *ScheduleService) UpdateChain(ctx context.Context, input UpdateChainInput) (SchedulesView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Schedules.EnsureDefaults(ctx, s.now().UTC().Format(timeFormat)); err != nil {
		return SchedulesView{}, err
	}
	chain, err := s.loadChain(ctx)
	if err != nil {
		return SchedulesView{}, err
	}
	if input.StartTime != nil {
		if err := validateClock(*input.StartTime); err != nil {
			return SchedulesView{}, err
		}
		chain.StartTime = *input.StartTime
	}
	if input.WeeklyScrubDay != nil {
		if *input.WeeklyScrubDay < 0 || *input.WeeklyScrubDay > 6 {
			return SchedulesView{}, fmt.Errorf("%w: weeklyScrubDay must be 0-6", ErrInvalidScheduleInput)
		}
		chain.WeeklyScrubDay = *input.WeeklyScrubDay
	}
	for step, enabled := range input.StepEnabled {
		chain.Enabled[step] = enabled
	}
	if err := s.persistChain(ctx, chain); err != nil {
		return SchedulesView{}, err
	}
	return s.getLocked(ctx)
}

// UpdateOtherJobInput carries optional fields for one separately scheduled job.
type UpdateOtherJobInput struct {
	Enabled   *bool
	Frequency *job.Frequency
	Time      *string
}

// UpdateOtherJob persists one separately scheduled job and returns the view.
func (s *ScheduleService) UpdateOtherJob(ctx context.Context, jobID string, input UpdateOtherJobInput) (SchedulesView, error) {
	if !ValidOtherJobID(jobID) {
		return SchedulesView{}, ErrScheduleJobNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Schedules.EnsureDefaults(ctx, s.now().UTC().Format(timeFormat)); err != nil {
		return SchedulesView{}, err
	}
	others, err := s.loadOtherJobs(ctx)
	if err != nil {
		return SchedulesView{}, err
	}
	var target *job.OtherJobSettings
	for i := range others {
		if others[i].ID == jobID {
			target = &others[i]
			break
		}
	}
	if target == nil {
		return SchedulesView{}, ErrScheduleJobNotFound
	}
	if input.Enabled != nil {
		target.Enabled = *input.Enabled
	}
	if input.Frequency != nil {
		if !validFrequency(*input.Frequency) {
			return SchedulesView{}, fmt.Errorf("%w: unknown frequency %q", ErrInvalidScheduleInput, *input.Frequency)
		}
		target.Frequency = *input.Frequency
	}
	if input.Time != nil {
		if err := validateClock(*input.Time); err != nil {
			return SchedulesView{}, err
		}
		target.Time = *input.Time
	}
	if err := s.Schedules.UpsertJob(ctx, ScheduleJobRow{
		JobID:     target.ID,
		Enabled:   target.Enabled,
		Frequency: string(target.Frequency),
		StartTime: target.Time,
		UpdatedAt: s.now().UTC().Format(timeFormat),
	}); err != nil {
		return SchedulesView{}, fmt.Errorf("schedule: saving job %s: %w", jobID, err)
	}
	return s.getLocked(ctx)
}

// ClaimedChain is one nightly window that ClaimDueChain has already
// persisted last-run for, so a restart inside the same window cannot
// start a second overlapping chain.
type ClaimedChain struct {
	Settings job.ChainSettings
	Location *time.Location
	At       time.Time
}

// ClaimDueChain returns a claimed window when today's start time has
// been reached and this night has not already been claimed. A nil result
// means the chain is not due. Last-run is recorded before the caller
// constructs MaintenanceChain.Run, not after it succeeds.
func (s *ScheduleService) ClaimDueChain(ctx context.Context) (*ClaimedChain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if err := s.Schedules.EnsureDefaults(ctx, now.UTC().Format(timeFormat)); err != nil {
		return nil, err
	}
	chain, lastRun, err := s.loadChainWithLastRun(ctx)
	if err != nil {
		return nil, err
	}
	loc := s.timezone(ctx)
	if !job.ChainIsDue(now, loc, chain.StartTime, lastRun) {
		return nil, nil
	}
	if err := s.Schedules.SetChainLastRun(ctx, now.UTC().Format(timeFormat)); err != nil {
		return nil, fmt.Errorf("schedule: claiming chain run: %w", err)
	}
	return &ClaimedChain{Settings: chain, Location: loc, At: now}, nil
}

func (s *ScheduleService) loadChainWithLastRun(ctx context.Context) (job.ChainSettings, *time.Time, error) {
	row, err := s.Schedules.GetChain(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return job.DefaultChainSettings(), nil, nil
		}
		return job.ChainSettings{}, nil, fmt.Errorf("schedule: loading chain: %w", err)
	}
	chain := chainFromRow(row)
	if row.LastRunAt == "" {
		return chain, nil, nil
	}
	t, err := time.Parse(timeFormat, row.LastRunAt)
	if err != nil {
		return job.ChainSettings{}, nil, fmt.Errorf("schedule: parsing chain last_run_at: %w", err)
	}
	return chain, &t, nil
}

func chainFromRow(row *ScheduleChainRow) job.ChainSettings {
	enabled := job.DefaultChainSettings().Enabled
	enabled[job.StepMover] = row.MoverEnabled
	enabled[job.StepDiffGuard] = row.DiffGuardEnabled
	enabled[job.StepSync] = row.SyncEnabled
	enabled[job.StepScrub] = row.ScrubEnabled
	enabled[job.StepConfigBackup] = row.ConfigBackupEnabled
	return job.ChainSettings{
		StartTime:      row.StartTime,
		WeeklyScrubDay: row.WeeklyScrubDay,
		Enabled:        enabled,
	}
}

func (s *ScheduleService) loadChain(ctx context.Context) (job.ChainSettings, error) {
	row, err := s.Schedules.GetChain(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return job.DefaultChainSettings(), nil
		}
		return job.ChainSettings{}, fmt.Errorf("schedule: loading chain: %w", err)
	}
	return chainFromRow(row), nil
}

func (s *ScheduleService) persistChain(ctx context.Context, chain job.ChainSettings) error {
	return s.Schedules.UpsertChain(ctx, ScheduleChainRow{
		StartTime:           chain.StartTime,
		WeeklyScrubDay:      chain.WeeklyScrubDay,
		MoverEnabled:        chain.Enabled[job.StepMover],
		DiffGuardEnabled:    chain.Enabled[job.StepDiffGuard],
		SyncEnabled:         chain.Enabled[job.StepSync],
		ScrubEnabled:        chain.Enabled[job.StepScrub],
		ConfigBackupEnabled: chain.Enabled[job.StepConfigBackup],
		UpdatedAt:           s.now().UTC().Format(timeFormat),
	})
}

func (s *ScheduleService) loadOtherJobs(ctx context.Context) ([]job.OtherJobSettings, error) {
	rows, err := s.Schedules.ListJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("schedule: loading jobs: %w", err)
	}
	if len(rows) == 0 {
		return job.DefaultOtherJobs(), nil
	}
	out := make([]job.OtherJobSettings, len(rows))
	for i, row := range rows {
		out[i] = job.OtherJobSettings{
			ID:        row.JobID,
			Enabled:   row.Enabled,
			Frequency: job.Frequency(row.Frequency),
			Time:      row.StartTime,
		}
	}
	return out, nil
}

func (s *ScheduleService) timezone(ctx context.Context) *time.Location {
	row, err := s.Settings.Get(ctx)
	if err != nil || row == nil || !row.Timezone.Valid || row.Timezone.String == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(row.Timezone.String)
	if err != nil {
		return time.UTC
	}
	return loc
}

func validateClock(value string) error {
	if _, _, err := job.ParseClock(value); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidScheduleInput, err)
	}
	return nil
}

func validFrequency(freq job.Frequency) bool {
	switch freq {
	case job.FrequencyDaily, job.FrequencyWeekly, job.FrequencyMonthly:
		return true
	default:
		return false
	}
}
