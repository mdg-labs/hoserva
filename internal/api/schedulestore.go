package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// ScheduleChainRow is schedule_chain's persisted nightly maintenance chain.
type ScheduleChainRow struct {
	StartTime           string
	WeeklyScrubDay      int
	MoverEnabled        bool
	DiffGuardEnabled    bool
	SyncEnabled         bool
	ScrubEnabled        bool
	ConfigBackupEnabled bool
	UpdatedAt           string
}

// ScheduleJobRow is one separately scheduled recurring job.
type ScheduleJobRow struct {
	JobID     string
	Enabled   bool
	Frequency string
	StartTime string
	UpdatedAt string
}

// ScheduleStore reads and writes schedule_chain and schedule_jobs.
type ScheduleStore struct {
	q *storedb.Queries
}

// NewScheduleStore wraps db for schedule persistence.
func NewScheduleStore(db storedb.DBTX) *ScheduleStore {
	return &ScheduleStore{q: storedb.New(db)}
}

// GetChain returns the persisted chain row, or sql.ErrNoRows when unset.
func (s *ScheduleStore) GetChain(ctx context.Context) (*ScheduleChainRow, error) {
	row, err := s.q.GetScheduleChain(ctx)
	if err != nil {
		return nil, err
	}
	return &ScheduleChainRow{
		StartTime:           row.StartTime,
		WeeklyScrubDay:      int(row.WeeklyScrubDay),
		MoverEnabled:        row.MoverEnabled != 0,
		DiffGuardEnabled:    row.DiffGuardEnabled != 0,
		SyncEnabled:         row.SyncEnabled != 0,
		ScrubEnabled:        row.ScrubEnabled != 0,
		ConfigBackupEnabled: row.ConfigBackupEnabled != 0,
		UpdatedAt:           row.UpdatedAt,
	}, nil
}

// UpsertChain persists the chain row.
func (s *ScheduleStore) UpsertChain(ctx context.Context, row ScheduleChainRow) error {
	return s.q.UpsertScheduleChain(ctx, storedb.UpsertScheduleChainParams{
		StartTime:           row.StartTime,
		WeeklyScrubDay:      int64(row.WeeklyScrubDay),
		MoverEnabled:        boolToInt(row.MoverEnabled),
		DiffGuardEnabled:    boolToInt(row.DiffGuardEnabled),
		SyncEnabled:         boolToInt(row.SyncEnabled),
		ScrubEnabled:        boolToInt(row.ScrubEnabled),
		ConfigBackupEnabled: boolToInt(row.ConfigBackupEnabled),
		UpdatedAt:           row.UpdatedAt,
	})
}

// ListJobs returns every separately scheduled job row.
func (s *ScheduleStore) ListJobs(ctx context.Context) ([]ScheduleJobRow, error) {
	rows, err := s.q.ListScheduleJobs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ScheduleJobRow, len(rows))
	for i, row := range rows {
		out[i] = ScheduleJobRow{
			JobID:     row.JobID,
			Enabled:   row.Enabled != 0,
			Frequency: row.Frequency,
			StartTime: row.StartTime,
			UpdatedAt: row.UpdatedAt,
		}
	}
	return out, nil
}

// UpsertJob persists one separately scheduled job row.
func (s *ScheduleStore) UpsertJob(ctx context.Context, row ScheduleJobRow) error {
	return s.q.UpsertScheduleJob(ctx, storedb.UpsertScheduleJobParams{
		JobID:     row.JobID,
		Enabled:   boolToInt(row.Enabled),
		Frequency: row.Frequency,
		StartTime: row.StartTime,
		UpdatedAt: row.UpdatedAt,
	})
}

func boolToInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

// ErrScheduleJobNotFound is returned when updating an unknown job id.
var ErrScheduleJobNotFound = errors.New("schedule: job not found")

// ValidOtherJobID reports whether id is one of the separately scheduled jobs.
func ValidOtherJobID(id string) bool {
	for _, job := range defaultOtherJobIDs {
		if job == id {
			return true
		}
	}
	return false
}

var defaultOtherJobIDs = []string{
	"smart_self_test",
	"appdata_backup",
	"restore_drill",
	"container_update_check",
}

// EnsureDefaults seeds schedule_chain and schedule_jobs when missing.
func (s *ScheduleStore) EnsureDefaults(ctx context.Context, now string) error {
	if _, err := s.GetChain(ctx); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("schedule: loading chain defaults: %w", err)
		}
		def := defaultChainRow(now)
		if err := s.UpsertChain(ctx, def); err != nil {
			return fmt.Errorf("schedule: seeding chain defaults: %w", err)
		}
	}
	existing, err := s.ListJobs(ctx)
	if err != nil {
		return fmt.Errorf("schedule: loading job defaults: %w", err)
	}
	existingIDs := make(map[string]struct{}, len(existing))
	for _, row := range existing {
		existingIDs[row.JobID] = struct{}{}
	}
	for _, job := range defaultOtherJobRows(now) {
		if _, ok := existingIDs[job.JobID]; ok {
			continue
		}
		if err := s.UpsertJob(ctx, job); err != nil {
			return fmt.Errorf("schedule: seeding job %s defaults: %w", job.JobID, err)
		}
	}
	return nil
}

func defaultChainRow(now string) ScheduleChainRow {
	return ScheduleChainRow{
		StartTime:           "02:00",
		WeeklyScrubDay:      0,
		MoverEnabled:        true,
		DiffGuardEnabled:    true,
		SyncEnabled:         true,
		ScrubEnabled:        true,
		ConfigBackupEnabled: true,
		UpdatedAt:           now,
	}
}

func defaultOtherJobRows(now string) []ScheduleJobRow {
	return []ScheduleJobRow{
		{JobID: "smart_self_test", Enabled: true, Frequency: "weekly", StartTime: "03:00", UpdatedAt: now},
		{JobID: "appdata_backup", Enabled: false, Frequency: "daily", StartTime: "04:00", UpdatedAt: now},
		{JobID: "restore_drill", Enabled: false, Frequency: "monthly", StartTime: "05:00", UpdatedAt: now},
		{JobID: "container_update_check", Enabled: true, Frequency: "daily", StartTime: "06:00", UpdatedAt: now},
	}
}
