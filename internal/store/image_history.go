package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// DefaultImageKeepDays is how long the image a container ran before an
// update is kept for a revert while no setting says otherwise.
const DefaultImageKeepDays = 7

// MaxImageKeepDays is the longest period the setting accepts.
const MaxImageKeepDays = 365

// ErrImageKeepDays is SetImageKeepDays's refusal of a period outside 1 to
// MaxImageKeepDays days.
var ErrImageKeepDays = errors.New("store: the image keep period must be between 1 and 365 days")

// ErrImageHistoryNotFound is MarkImageHistoryReverted when no unreverted row
// has that id.
var ErrImageHistoryNotFound = errors.New("store: no such container update record, or it was already reverted")

// ImageHistory is one row of container_image_history: what reverting one
// container update needs.
type ImageHistory struct {
	ID                  int64
	Container           string
	Image               string
	PreviousImageID     string
	SnapshotArchive     string
	SnapshotDestination string
	UpdatedAt           time.Time
	KeepUntil           time.Time
	RevertedAt          time.Time // zero until a revert has put the previous image back
}

// ImageHistoryStore persists container update history, the bulk-update
// opt-out and the update settings in the central SQLite database (D4).
type ImageHistoryStore struct {
	q *storedb.Queries
}

// NewImageHistoryStore wraps db for container update persistence.
func NewImageHistoryStore(db storedb.DBTX) *ImageHistoryStore {
	return &ImageHistoryStore{q: storedb.New(db)}
}

// InsertImageHistory creates the row for h and returns it with its ID. Its
// RevertedAt is always empty.
func (s *ImageHistoryStore) InsertImageHistory(ctx context.Context, h ImageHistory) (ImageHistory, error) {
	id, err := s.q.InsertContainerImageHistory(ctx, storedb.InsertContainerImageHistoryParams{
		Container:           h.Container,
		Image:               h.Image,
		PreviousImageID:     h.PreviousImageID,
		SnapshotArchive:     h.SnapshotArchive,
		SnapshotDestination: h.SnapshotDestination,
		UpdatedAt:           h.UpdatedAt.UTC().Format(TimeFormat),
		KeepUntil:           h.KeepUntil.UTC().Format(TimeFormat),
	})
	if err != nil {
		return ImageHistory{}, fmt.Errorf("store: recording the update of %s: %w", h.Container, err)
	}
	h.ID = id
	h.RevertedAt = time.Time{}
	return h, nil
}

// LatestImageHistory returns the newest row of the container, reverted or
// not, and false when it has none.
func (s *ImageHistoryStore) LatestImageHistory(ctx context.Context, container string) (ImageHistory, bool, error) {
	row, err := s.q.GetLatestContainerImageHistory(ctx, container)
	if errors.Is(err, sql.ErrNoRows) {
		return ImageHistory{}, false, nil
	}
	if err != nil {
		return ImageHistory{}, false, fmt.Errorf("store: reading the update history of %s: %w", container, err)
	}
	h, err := imageHistoryFromRow(row)
	return h, err == nil, err
}

// ListImageHistory returns every row, newest first.
func (s *ImageHistoryStore) ListImageHistory(ctx context.Context) ([]ImageHistory, error) {
	rows, err := s.q.ListContainerImageHistory(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing the container update history: %w", err)
	}
	out := make([]ImageHistory, 0, len(rows))
	for _, row := range rows {
		h, err := imageHistoryFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

func imageHistoryFromRow(row *storedb.ContainerImageHistory) (ImageHistory, error) {
	h := ImageHistory{
		ID: row.ID, Container: row.Container, Image: row.Image, PreviousImageID: row.PreviousImageID,
		SnapshotArchive: row.SnapshotArchive, SnapshotDestination: row.SnapshotDestination,
	}
	var err error
	if h.UpdatedAt, err = time.Parse(TimeFormat, row.UpdatedAt); err != nil {
		return ImageHistory{}, fmt.Errorf("store: parsing updated_at of update record %d: %w", row.ID, err)
	}
	if h.KeepUntil, err = time.Parse(TimeFormat, row.KeepUntil); err != nil {
		return ImageHistory{}, fmt.Errorf("store: parsing keep_until of update record %d: %w", row.ID, err)
	}
	if row.RevertedAt != "" {
		if h.RevertedAt, err = time.Parse(TimeFormat, row.RevertedAt); err != nil {
			return ImageHistory{}, fmt.Errorf("store: parsing reverted_at of update record %d: %w", row.ID, err)
		}
	}
	return h, nil
}

// MarkImageHistoryReverted records that row id was reverted at at. It
// returns ErrImageHistoryNotFound for a row that does not exist or was
// reverted already.
func (s *ImageHistoryStore) MarkImageHistoryReverted(ctx context.Context, id int64, at time.Time) error {
	n, err := s.q.MarkContainerImageHistoryReverted(ctx, storedb.MarkContainerImageHistoryRevertedParams{
		RevertedAt: at.UTC().Format(TimeFormat),
		ID:         id,
	})
	if err != nil {
		return fmt.Errorf("store: marking update record %d reverted: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %d", ErrImageHistoryNotFound, id)
	}
	return nil
}

// DeleteImageHistory removes row id; a missing row is not an error.
func (s *ImageHistoryStore) DeleteImageHistory(ctx context.Context, id int64) error {
	if err := s.q.DeleteContainerImageHistory(ctx, id); err != nil {
		return fmt.Errorf("store: deleting update record %d: %w", id, err)
	}
	return nil
}

// SetBulkExcluded sets whether a bulk update skips the container.
func (s *ImageHistoryStore) SetBulkExcluded(ctx context.Context, container string, excluded bool) error {
	var v int64
	if excluded {
		v = 1
	}
	if err := s.q.SetContainerUpdatePolicy(ctx, storedb.SetContainerUpdatePolicyParams{Container: container, BulkExcluded: v}); err != nil {
		return fmt.Errorf("store: saving the bulk update policy of %s: %w", container, err)
	}
	return nil
}

// BulkExcluded returns the names of the containers a bulk update skips, in
// name order.
func (s *ImageHistoryStore) BulkExcluded(ctx context.Context) ([]string, error) {
	names, err := s.q.ListBulkExcludedContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing the containers excluded from bulk updates: %w", err)
	}
	if names == nil {
		names = []string{}
	}
	return names, nil
}

// ImageKeepDays returns how many days the previous image is kept for a
// revert: the stored setting, or DefaultImageKeepDays when none is stored.
func (s *ImageHistoryStore) ImageKeepDays(ctx context.Context) (int, error) {
	days, err := s.q.GetContainerUpdateSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultImageKeepDays, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: reading the container update settings: %w", err)
	}
	return int(days), nil
}

// SetImageKeepDays stores the keep period; it refuses (ErrImageKeepDays) one
// outside 1 to MaxImageKeepDays days.
func (s *ImageHistoryStore) SetImageKeepDays(ctx context.Context, days int) error {
	if days < 1 || days > MaxImageKeepDays {
		return fmt.Errorf("%w: %d", ErrImageKeepDays, days)
	}
	if err := s.q.SetContainerUpdateSettings(ctx, int64(days)); err != nil {
		return fmt.Errorf("store: saving the container update settings: %w", err)
	}
	return nil
}
