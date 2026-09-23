package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// Store persists jobs in the central SQLite database (D4, doc 01 §4)
// through the sqlc-generated internal/store/db package. It has no
// business logic of its own — Scheduler owns exclusion, checkpoints and
// status transitions; Store only reads and writes rows.
type Store struct {
	q *storedb.Queries
}

// NewStore wraps db (typically *sql.DB, or a *sql.Tx via WithTx) for job
// persistence.
func NewStore(db storedb.DBTX) *Store {
	return &Store{q: storedb.New(db)}
}

// Create inserts j as its first row. j.CreatedAt must already be set.
func (s *Store) Create(ctx context.Context, j *Job) error {
	resourceIDs, err := encodeResourceIDs(j.ResourceIDs)
	if err != nil {
		return fmt.Errorf("job store: encoding resource ids: %w", err)
	}
	return s.q.CreateJob(ctx, storedb.CreateJobParams{
		ID:           j.ID,
		Type:         string(j.Type),
		Class:        string(j.Class),
		Status:       string(j.Status),
		Progress:     progressToSQL(j.Progress),
		Resumable:    boolToSQL(j.Resumable),
		Cancellable:  boolToSQL(j.Cancellable),
		ResourceIds:  resourceIDs,
		Checkpoint:   j.Checkpoint,
		Params:       paramsToSQL(j.Params),
		ErrorCode:    stringToSQL(j.ErrorCode),
		ErrorMessage: stringToSQL(j.ErrorMessage),
		CreatedAt:    j.CreatedAt.Format(store.TimeFormat),
		StartedAt:    timeToSQL(j.StartedAt),
		FinishedAt:   timeToSQL(j.FinishedAt),
	})
}

// ErrNotFound is returned by Get when no job has the requested id.
var ErrNotFound = sql.ErrNoRows

// Get returns the job with the given id, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	row, err := s.q.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	return fromRow(row)
}

// List returns jobs matching filter, newest first (listJobs, doc 01 §4).
func (s *Store) List(ctx context.Context, filter ListFilter) ([]*Job, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.q.ListJobs(ctx, storedb.ListJobsParams{
		Class:    classFilterArg(filter.Class),
		Status:   statusFilterArg(filter.Status),
		RowLimit: int64(limit),
	})
	if err != nil {
		return nil, err
	}
	return fromRows(rows)
}

// ListActive returns every job currently queued or running — what the
// scheduler recovers at startup and checks new submissions against
// (doc 01 §4).
func (s *Store) ListActive(ctx context.Context) ([]*Job, error) {
	rows, err := s.q.ListActiveJobs(ctx)
	if err != nil {
		return nil, err
	}
	return fromRows(rows)
}

// ListPending returns every job of type t that is queued, running or
// interrupted, oldest first — every job of that type that has not ended.
func (s *Store) ListPending(ctx context.Context, t Type) ([]*Job, error) {
	rows, err := s.q.ListPendingJobsOfType(ctx, string(t))
	if err != nil {
		return nil, err
	}
	return fromRows(rows)
}

// SetCancellable persists whether the job with id accepts a cancel.
func (s *Store) SetCancellable(ctx context.Context, id string, cancellable bool) error {
	return s.q.SetJobCancellable(ctx, storedb.SetJobCancellableParams{
		Cancellable: boolToSQL(cancellable),
		ID:          id,
	})
}

// UpdateStatus persists a job's terminal or in-flight state transition.
func (s *Store) UpdateStatus(ctx context.Context, id string, status Status, progress *int, errCode, errMessage string, startedAt, finishedAt *time.Time) error {
	return s.q.UpdateJobStatus(ctx, storedb.UpdateJobStatusParams{
		Status:       string(status),
		Progress:     progressToSQL(progress),
		ErrorCode:    stringToSQL(errCode),
		ErrorMessage: stringToSQL(errMessage),
		StartedAt:    timeToSQL(startedAt),
		FinishedAt:   timeToSQL(finishedAt),
		ID:           id,
	})
}

// UpdateProgress persists a running job's latest percentage.
func (s *Store) UpdateProgress(ctx context.Context, id string, progress *int) error {
	return s.q.UpdateJobProgress(ctx, storedb.UpdateJobProgressParams{
		Progress: progressToSQL(progress),
		ID:       id,
	})
}

// SaveCheckpoint persists a resumable job's latest checkpoint (Q29).
func (s *Store) SaveCheckpoint(ctx context.Context, id string, data []byte) error {
	return s.q.SaveJobCheckpoint(ctx, storedb.SaveJobCheckpointParams{
		Checkpoint: data,
		ID:         id,
	})
}

// InterruptActive marks every queued/running job interrupted, as of at.
// Called once, at daemon startup (doc 01 §4: "never automatically
// resumed").
func (s *Store) InterruptActive(ctx context.Context, at time.Time) error {
	return s.q.InterruptActiveJobs(ctx, timeToSQL(&at))
}

func fromRows(rows []*storedb.Job) ([]*Job, error) {
	out := make([]*Job, 0, len(rows))
	for _, row := range rows {
		j, err := fromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}

func fromRow(row *storedb.Job) (*Job, error) {
	resourceIDs, err := decodeResourceIDs(row.ResourceIds)
	if err != nil {
		return nil, fmt.Errorf("job store: decoding resource ids for %s: %w", row.ID, err)
	}
	createdAt, err := time.Parse(store.TimeFormat, row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("job store: parsing created_at for %s: %w", row.ID, err)
	}
	startedAt, err := sqlToTime(row.StartedAt)
	if err != nil {
		return nil, fmt.Errorf("job store: parsing started_at for %s: %w", row.ID, err)
	}
	finishedAt, err := sqlToTime(row.FinishedAt)
	if err != nil {
		return nil, fmt.Errorf("job store: parsing finished_at for %s: %w", row.ID, err)
	}
	return &Job{
		ID:           row.ID,
		Type:         Type(row.Type),
		Class:        Class(row.Class),
		Status:       Status(row.Status),
		Progress:     sqlToProgress(row.Progress),
		Resumable:    row.Resumable != 0,
		Cancellable:  row.Cancellable != 0,
		ResourceIDs:  resourceIDs,
		Checkpoint:   row.Checkpoint,
		Params:       sqlToParams(row.Params),
		ErrorCode:    row.ErrorCode.String,
		ErrorMessage: row.ErrorMessage.String,
		CreatedAt:    createdAt,
		StartedAt:    startedAt,
		FinishedAt:   finishedAt,
	}, nil
}

func encodeResourceIDs(ids []string) (sql.NullString, error) {
	if len(ids) == 0 {
		return sql.NullString{}, nil
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(data), Valid: true}, nil
}

func decodeResourceIDs(v sql.NullString) ([]string, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(v.String), &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func paramsToSQL(p []byte) sql.NullString {
	if len(p) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(p), Valid: true}
}

func sqlToParams(v sql.NullString) []byte {
	if !v.Valid || v.String == "" {
		return nil
	}
	return []byte(v.String)
}

func progressToSQL(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}

func sqlToProgress(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	p := int(v.Int64)
	return &p
}

func boolToSQL(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func stringToSQL(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func timeToSQL(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: t.Format(store.TimeFormat), Valid: true}
}

func sqlToTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	t, err := time.Parse(store.TimeFormat, v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func classFilterArg(c *Class) any {
	if c == nil {
		return nil
	}
	return string(*c)
}

func statusFilterArg(s *Status) any {
	if s == nil {
		return nil
	}
	return string(*s)
}
