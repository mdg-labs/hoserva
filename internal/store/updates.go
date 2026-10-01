package store

import (
	"context"
	"fmt"
	"time"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// Update check outcomes (image_update_checks.status and .kind).
const (
	UpdateUpToDate       = "up_to_date"
	UpdateAvailable      = "update_available"
	UpdateSkipped        = "skipped"
	UpdateFailed         = "failed"
	UpdateNotChecked     = "not_checked"
	UpdateKindNewBuild   = "new_build"
	UpdateKindNewVersion = "new_version"
)

// ImageUpdateCheck is one row of image_update_checks: what the last update
// check found for an image ("repository:tag").
type ImageUpdateCheck struct {
	Image        string
	CheckedAt    time.Time
	Status       string
	Kind         string
	AvailableTag string
	Message      string
}

// UpdateStore persists update check results in the
// central SQLite database (D4).
type UpdateStore struct {
	q *storedb.Queries
}

// NewUpdateStore wraps db for update check persistence.
func NewUpdateStore(db storedb.DBTX) *UpdateStore {
	return &UpdateStore{q: storedb.New(db)}
}

// PutImageUpdateCheck creates or replaces the result for c.Image.
func (s *UpdateStore) PutImageUpdateCheck(ctx context.Context, c ImageUpdateCheck) error {
	err := s.q.UpsertImageUpdateCheck(ctx, storedb.UpsertImageUpdateCheckParams{
		Image:        c.Image,
		CheckedAt:    c.CheckedAt.UTC().Format(TimeFormat),
		Status:       c.Status,
		Kind:         c.Kind,
		AvailableTag: c.AvailableTag,
		Message:      c.Message,
	})
	if err != nil {
		return fmt.Errorf("store: saving the update check of %s: %w", c.Image, err)
	}
	return nil
}

// ListImageUpdateChecks returns every stored result, sorted by image.
func (s *UpdateStore) ListImageUpdateChecks(ctx context.Context) ([]ImageUpdateCheck, error) {
	rows, err := s.q.ListImageUpdateChecks(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing update checks: %w", err)
	}
	out := make([]ImageUpdateCheck, 0, len(rows))
	for _, row := range rows {
		at, err := time.Parse(TimeFormat, row.CheckedAt)
		if err != nil {
			return nil, fmt.Errorf("store: parsing the update check of %s checked_at: %w", row.Image, err)
		}
		out = append(out, ImageUpdateCheck{Image: row.Image, CheckedAt: at, Status: row.Status, Kind: row.Kind, AvailableTag: row.AvailableTag, Message: row.Message})
	}
	return out, nil
}

// DeleteImageUpdateCheck removes the result for image; a missing row is not
// an error.
func (s *UpdateStore) DeleteImageUpdateCheck(ctx context.Context, image string) error {
	if err := s.q.DeleteImageUpdateCheck(ctx, image); err != nil {
		return fmt.Errorf("store: deleting the update check of %s: %w", image, err)
	}
	return nil
}
