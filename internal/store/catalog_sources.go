package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// CatalogSourceKind says where a catalog source came from.
type CatalogSourceKind string

const (
	// CatalogSourceCurated is Hoserva's own catalog (doc 04 §7).
	CatalogSourceCurated CatalogSourceKind = "curated"
	// CatalogSourceUserAdded is a source URL the user added (doc 04 §4).
	CatalogSourceUserAdded CatalogSourceKind = "user_added"
)

var (
	// ErrCatalogSourceNotFound is Get, RecordRefresh and DeleteUserAdded
	// when no row has that id.
	ErrCatalogSourceNotFound = errors.New("store: catalog source not found")
	// ErrCatalogSourceExists is Insert's refusal of a source whose id or
	// URL is already used.
	ErrCatalogSourceExists = errors.New("store: catalog source already exists")
	// ErrCatalogSourceCurated is DeleteUserAdded's refusal to remove the
	// curated catalog.
	ErrCatalogSourceCurated = errors.New("store: the curated catalog source cannot be removed")
)

// CatalogSource is one row of the catalog_sources table (#283). PublicKey is
// the base64 Ed25519 key of a user-added source, empty when it is unsigned.
// SignatureVerified is true only while the installed copy passed a signature
// check, which the table forbids for a user-added source with no key.
// LastRefreshedAt is the zero time until the source has been fetched once.
type CatalogSource struct {
	ID                string
	URL               string
	Kind              CatalogSourceKind
	PublicKey         string
	SignatureVerified bool
	LastRefreshedAt   time.Time
	AddedAt           time.Time
}

// CatalogSourceStore persists catalog sources in the central SQLite database
// (D4).
type CatalogSourceStore struct {
	q *storedb.Queries
}

// NewCatalogSourceStore wraps db for catalog source persistence.
func NewCatalogSourceStore(db storedb.DBTX) *CatalogSourceStore {
	return &CatalogSourceStore{q: storedb.New(db)}
}

func formatSourceTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(TimeFormat)
}

// Insert creates a source row. It refuses (ErrCatalogSourceExists) an id or
// URL that is already used, and a user-added source with no public key that
// is marked signature-verified.
func (s *CatalogSourceStore) Insert(ctx context.Context, src CatalogSource) error {
	err := s.q.InsertCatalogSource(ctx, storedb.InsertCatalogSourceParams{
		ID:                src.ID,
		Url:               src.URL,
		Kind:              string(src.Kind),
		PublicKey:         src.PublicKey,
		SignatureVerified: boolToInt(src.SignatureVerified),
		LastRefreshedAt:   formatSourceTime(src.LastRefreshedAt),
		AddedAt:           formatSourceTime(src.AddedAt),
	})
	if isUniqueConstraint(err) {
		return fmt.Errorf("%w: %s", ErrCatalogSourceExists, src.URL)
	}
	if err != nil {
		return fmt.Errorf("store: inserting catalog source %s: %w", src.ID, err)
	}
	return nil
}

// EnsureCurated writes the curated catalog's row when it is missing and
// leaves an existing one exactly as it is.
func (s *CatalogSourceStore) EnsureCurated(ctx context.Context, id, url string, at time.Time) error {
	err := s.q.EnsureCuratedCatalogSource(ctx, storedb.EnsureCuratedCatalogSourceParams{ID: id, Url: url, AddedAt: formatSourceTime(at)})
	if err != nil {
		return fmt.Errorf("store: recording the curated catalog source: %w", err)
	}
	return nil
}

// Get returns the source with that id, or ErrCatalogSourceNotFound.
func (s *CatalogSourceStore) Get(ctx context.Context, id string) (CatalogSource, error) {
	row, err := s.q.GetCatalogSource(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return CatalogSource{}, fmt.Errorf("%w: %s", ErrCatalogSourceNotFound, id)
	}
	if err != nil {
		return CatalogSource{}, fmt.Errorf("store: getting catalog source %s: %w", id, err)
	}
	return catalogSourceFromRow(row)
}

// List returns every source: the curated catalog first, then the user-added
// ones in the order they were added.
func (s *CatalogSourceStore) List(ctx context.Context) ([]CatalogSource, error) {
	rows, err := s.q.ListCatalogSources(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing catalog sources: %w", err)
	}
	out := make([]CatalogSource, 0, len(rows))
	for _, row := range rows {
		src, err := catalogSourceFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, nil
}

// RecordRefresh sets a source's refresh time and signature-verified flag in
// one statement, leaving every other column as it is. The table refuses
// verified for a user-added source with no key. It refuses
// (ErrCatalogSourceNotFound) a missing id.
func (s *CatalogSourceStore) RecordRefresh(ctx context.Context, id string, verified bool, at time.Time) error {
	n, err := s.q.RecordCatalogSourceRefresh(ctx, storedb.RecordCatalogSourceRefreshParams{
		LastRefreshedAt:   formatSourceTime(at),
		SignatureVerified: boolToInt(verified),
		ID:                id,
	})
	if err != nil {
		return fmt.Errorf("store: recording the refresh of catalog source %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrCatalogSourceNotFound, id)
	}
	return nil
}

// DeleteUserAdded removes one user-added source's row and nothing else. It
// refuses (ErrCatalogSourceCurated) the curated catalog and
// (ErrCatalogSourceNotFound) a missing id.
func (s *CatalogSourceStore) DeleteUserAdded(ctx context.Context, id string) error {
	n, err := s.q.DeleteUserAddedCatalogSource(ctx, id)
	if err != nil {
		return fmt.Errorf("store: deleting catalog source %s: %w", id, err)
	}
	if n > 0 {
		return nil
	}
	src, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if src.Kind == CatalogSourceCurated {
		return fmt.Errorf("%w: %s", ErrCatalogSourceCurated, id)
	}
	return fmt.Errorf("%w: %s", ErrCatalogSourceNotFound, id)
}

func catalogSourceFromRow(row *storedb.CatalogSource) (CatalogSource, error) {
	added, err := time.Parse(TimeFormat, row.AddedAt)
	if err != nil {
		return CatalogSource{}, fmt.Errorf("store: parsing catalog source %s added_at: %w", row.ID, err)
	}
	var refreshed time.Time
	if row.LastRefreshedAt != "" {
		if refreshed, err = time.Parse(TimeFormat, row.LastRefreshedAt); err != nil {
			return CatalogSource{}, fmt.Errorf("store: parsing catalog source %s last_refreshed_at: %w", row.ID, err)
		}
	}
	return CatalogSource{
		ID:                row.ID,
		URL:               row.Url,
		Kind:              CatalogSourceKind(row.Kind),
		PublicKey:         row.PublicKey,
		SignatureVerified: row.SignatureVerified != 0,
		LastRefreshedAt:   refreshed,
		AddedAt:           added,
	}, nil
}
