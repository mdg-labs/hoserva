package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrGroupExists is CreateGroup's refusal of a duplicate name.
	ErrGroupExists = errors.New("a group with that name already exists")
	// ErrGroupNotFound is DeleteGroup/SetGroupMembers naming an unknown
	// group.
	ErrGroupNotFound = errors.New("no such group")
)

// Group is a user_groups row (#49, Q27, doc 03 §7): a named collection of
// accounts kept purely for bulk share-permission assignment.
type Group struct {
	ID        string
	Name      string
	CreatedAt time.Time
	MemberIDs []string
}

// CreateGroup creates a new, empty group.
func (s *AuthService) CreateGroup(ctx context.Context, name string) (*Group, error) {
	g := &Group{ID: uuid.NewString(), Name: name, CreatedAt: s.Now()}
	if err := s.Store.CreateGroup(ctx, g.ID, g.Name, g.CreatedAt); err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrGroupExists
		}
		return nil, fmt.Errorf("creating group: %w", err)
	}
	return g, nil
}

// ListGroups returns every group, sorted by name, each with its member
// user ids.
func (s *AuthService) ListGroups(ctx context.Context) ([]*Group, error) {
	return s.Store.ListGroups(ctx)
}

// DeleteGroup removes the group along with its memberships and its
// per-share permissions.
func (s *AuthService) DeleteGroup(ctx context.Context, id string) error {
	return s.Store.DeleteGroup(ctx, id)
}

// SetGroupMembers replaces the group's membership with exactly userIDs.
// It refuses (ErrUserNotFound) any id that names no account.
func (s *AuthService) SetGroupMembers(ctx context.Context, id string, userIDs []string) (*Group, error) {
	if err := s.Store.SetGroupMembers(ctx, id, userIDs); err != nil {
		return nil, err
	}
	return s.Store.GetGroupByID(ctx, id)
}
