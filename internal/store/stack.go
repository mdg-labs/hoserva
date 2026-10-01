package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

var (
	// ErrStackNotFound is StackStore.Get and Delete when no row has that
	// name.
	ErrStackNotFound = errors.New("store: stack not found")
	// ErrStackExists is StackStore.Insert's refusal of a duplicate name.
	ErrStackExists = errors.New("store: stack already exists")
)

// Stack is one row of the stacks table (#278): what a stack's
// docker-compose.yml, .env and meta.json are generated from. SealedEnv is
// the .env text under the machine key; the store never sees it in the
// clear.
type Stack struct {
	Name             string
	TemplateSource   string
	TemplateID       string
	TemplateRevision string
	Compose          string
	SealedEnv        []byte
	InstalledAt      time.Time
	// ManuallyEdited is true once the Compose text was saved by hand, so a
	// template form must not overwrite it.
	ManuallyEdited bool
}

// StackStore persists stacks in the central SQLite database (D4).
type StackStore struct {
	q *storedb.Queries
}

// NewStackStore wraps db for stack persistence.
func NewStackStore(db storedb.DBTX) *StackStore {
	return &StackStore{q: storedb.New(db)}
}

// Insert creates a stack row. It refuses (ErrStackExists) a duplicate name.
func (s *StackStore) Insert(ctx context.Context, st Stack) error {
	if st.SealedEnv == nil {
		st.SealedEnv = []byte{}
	}
	err := s.q.InsertStack(ctx, storedb.InsertStackParams{
		Name:             st.Name,
		TemplateSource:   st.TemplateSource,
		TemplateID:       st.TemplateID,
		TemplateRevision: st.TemplateRevision,
		Compose:          st.Compose,
		Env:              st.SealedEnv,
		InstalledAt:      st.InstalledAt.UTC().Format(TimeFormat),
		ManuallyEdited:   boolToInt(st.ManuallyEdited),
	})
	if isUniqueConstraint(err) {
		return fmt.Errorf("%w: %s", ErrStackExists, st.Name)
	}
	if err != nil {
		return fmt.Errorf("store: inserting stack %s: %w", st.Name, err)
	}
	return nil
}

// Get returns the stack named name, or ErrStackNotFound.
func (s *StackStore) Get(ctx context.Context, name string) (Stack, error) {
	row, err := s.q.GetStack(ctx, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Stack{}, fmt.Errorf("%w: %s", ErrStackNotFound, name)
		}
		return Stack{}, fmt.Errorf("store: getting stack %s: %w", name, err)
	}
	return stackFromRow(row)
}

// List returns every stack, sorted by name.
func (s *StackStore) List(ctx context.Context) ([]Stack, error) {
	rows, err := s.q.ListStacks(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing stacks: %w", err)
	}
	out := make([]Stack, 0, len(rows))
	for _, row := range rows {
		st, err := stackFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// UpdateCompose sets the stack's Compose text and its manually-edited flag
// in one statement, leaving every other column as it is. It refuses
// (ErrStackNotFound) a missing name.
func (s *StackStore) UpdateCompose(ctx context.Context, name, compose string, manuallyEdited bool) error {
	n, err := s.q.UpdateStackCompose(ctx, storedb.UpdateStackComposeParams{
		Compose:        compose,
		ManuallyEdited: boolToInt(manuallyEdited),
		Name:           name,
	})
	if err != nil {
		return fmt.Errorf("store: updating the compose file of stack %s: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrStackNotFound, name)
	}
	return nil
}

// Delete removes the stack's row, never anything on disk. It refuses
// (ErrStackNotFound) a missing name.
func (s *StackStore) Delete(ctx context.Context, name string) error {
	n, err := s.q.DeleteStack(ctx, name)
	if err != nil {
		return fmt.Errorf("store: deleting stack %s: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrStackNotFound, name)
	}
	return nil
}

func stackFromRow(row *storedb.Stack) (Stack, error) {
	installed, err := time.Parse(TimeFormat, row.InstalledAt)
	if err != nil {
		return Stack{}, fmt.Errorf("store: parsing stack %s installed_at: %w", row.Name, err)
	}
	return Stack{
		Name:             row.Name,
		TemplateSource:   row.TemplateSource,
		TemplateID:       row.TemplateID,
		TemplateRevision: row.TemplateRevision,
		Compose:          row.Compose,
		SealedEnv:        row.Env,
		InstalledAt:      installed,
		ManuallyEdited:   row.ManuallyEdited != 0,
	}, nil
}
