package api

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func errAuthNotConfigured() error {
	return fmt.Errorf("auth service not configured")
}

func userSummaryToAPI(u *User) apiv1.UserSummary {
	return apiv1.UserSummary{
		ID:           uuid.MustParse(u.ID),
		Username:     u.Username,
		Role:         apiv1.UserRole(u.Role),
		TotpEnrolled: u.TOTPEnrolled(),
		CreatedAt:    u.CreatedAt,
	}
}

func groupToAPI(g *Group) apiv1.UserGroup {
	ids := make([]uuid.UUID, 0, len(g.MemberIDs))
	for _, id := range g.MemberIDs {
		ids = append(ids, uuid.MustParse(id))
	}
	return apiv1.UserGroup{ID: uuid.MustParse(g.ID), Name: g.Name, MemberUserIds: ids}
}

func (h *Handler) ListUsers(ctx context.Context) (*apiv1.ListUsersOK, error) {
	if h.Auth == nil {
		return nil, errAuthNotConfigured()
	}
	users, err := h.Auth.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	out := make([]apiv1.UserSummary, 0, len(users))
	for _, u := range users {
		out = append(out, userSummaryToAPI(u))
	}
	return &apiv1.ListUsersOK{Users: out}, nil
}

func (h *Handler) CreateUser(ctx context.Context, req *apiv1.CreateUserRequest) (*apiv1.UserSummary, error) {
	if h.Auth == nil {
		return nil, errAuthNotConfigured()
	}
	role := ""
	if r, ok := req.Role.Get(); ok {
		role = string(r)
	}
	u, err := h.Auth.CreateUser(ctx, req.Username, role)
	if err != nil {
		return nil, mapAuthError(err)
	}
	out := userSummaryToAPI(u)
	return &out, nil
}

func (h *Handler) UpdateUser(ctx context.Context, req *apiv1.UpdateUserRequest, params apiv1.UpdateUserParams) (*apiv1.UserSummary, error) {
	if h.Auth == nil {
		return nil, errAuthNotConfigured()
	}
	u, err := h.Auth.UpdateUserRole(ctx, params.UserId.String(), string(req.Role))
	if err != nil {
		return nil, mapAuthError(err)
	}
	out := userSummaryToAPI(u)
	return &out, nil
}

func (h *Handler) DeleteUser(ctx context.Context, params apiv1.DeleteUserParams) error {
	if h.Auth == nil {
		return errAuthNotConfigured()
	}
	if err := h.Auth.DeleteUser(ctx, params.UserId.String()); err != nil {
		return mapAuthError(err)
	}
	return nil
}

func (h *Handler) SetUserPassword(ctx context.Context, req *apiv1.SetUserPasswordRequest, params apiv1.SetUserPasswordParams) error {
	if h.Auth == nil {
		return errAuthNotConfigured()
	}
	if _, err := h.Auth.SetUserPassword(ctx, params.UserId.String(), req.Password); err != nil {
		return mapAuthError(err)
	}
	return nil
}

func userSharePermissionsToAPI(entries []UserSharePermission) []apiv1.UserSharePermission {
	out := make([]apiv1.UserSharePermission, 0, len(entries))
	for _, e := range entries {
		out = append(out, apiv1.UserSharePermission{
			ShareName: apiv1.ShareName(e.ShareName),
			Access:    apiv1.ShareAccessLevel(e.Access),
		})
	}
	return out
}

func (h *Handler) GetUserSharePermissions(ctx context.Context, params apiv1.GetUserSharePermissionsParams) (*apiv1.UserSharePermissionsResult, error) {
	if h.Auth == nil {
		return nil, errAuthNotConfigured()
	}
	entries, err := h.Auth.GetUserSharePermissions(ctx, params.UserId.String())
	if err != nil {
		return nil, mapAuthError(err)
	}
	return &apiv1.UserSharePermissionsResult{Permissions: userSharePermissionsToAPI(entries)}, nil
}

func (h *Handler) UpdateUserSharePermissions(ctx context.Context, req *apiv1.UpdateUserSharePermissionsRequest, params apiv1.UpdateUserSharePermissionsParams) (*apiv1.UserSharePermissionsResult, error) {
	if h.Auth == nil {
		return nil, errAuthNotConfigured()
	}
	entries := make([]UserSharePermission, 0, len(req.Permissions))
	for _, p := range req.Permissions {
		entries = append(entries, UserSharePermission{ShareName: string(p.ShareName), Access: string(p.Access)})
	}
	if err := h.Auth.SetUserSharePermissions(ctx, params.UserId.String(), entries); err != nil {
		return nil, mapAuthError(err)
	}
	updated, err := h.Auth.GetUserSharePermissions(ctx, params.UserId.String())
	if err != nil {
		return nil, mapAuthError(err)
	}
	return &apiv1.UserSharePermissionsResult{Permissions: userSharePermissionsToAPI(updated)}, nil
}

func (h *Handler) ListUserGroups(ctx context.Context) (*apiv1.ListUserGroupsOK, error) {
	if h.Auth == nil {
		return nil, errAuthNotConfigured()
	}
	groups, err := h.Auth.ListGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing groups: %w", err)
	}
	out := make([]apiv1.UserGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, groupToAPI(g))
	}
	return &apiv1.ListUserGroupsOK{Groups: out}, nil
}

func (h *Handler) CreateUserGroup(ctx context.Context, req *apiv1.CreateUserGroupRequest) (*apiv1.UserGroup, error) {
	if h.Auth == nil {
		return nil, errAuthNotConfigured()
	}
	g, err := h.Auth.CreateGroup(ctx, req.Name)
	if err != nil {
		return nil, mapAuthError(err)
	}
	out := groupToAPI(g)
	return &out, nil
}

func (h *Handler) DeleteUserGroup(ctx context.Context, params apiv1.DeleteUserGroupParams) error {
	if h.Auth == nil {
		return errAuthNotConfigured()
	}
	if err := h.Auth.DeleteGroup(ctx, params.GroupId.String()); err != nil {
		return mapAuthError(err)
	}
	return nil
}

func (h *Handler) SetUserGroupMembers(ctx context.Context, req *apiv1.SetUserGroupMembersRequest, params apiv1.SetUserGroupMembersParams) (*apiv1.UserGroup, error) {
	if h.Auth == nil {
		return nil, errAuthNotConfigured()
	}
	ids := make([]string, 0, len(req.UserIds))
	for _, id := range req.UserIds {
		ids = append(ids, id.String())
	}
	g, err := h.Auth.SetGroupMembers(ctx, params.GroupId.String(), ids)
	if err != nil {
		return nil, mapAuthError(err)
	}
	out := groupToAPI(g)
	return &out, nil
}

func (h *Handler) ListSessions(ctx context.Context) (*apiv1.ListSessionsOK, error) {
	if h.Auth == nil {
		return nil, errAuthNotConfigured()
	}
	sessions, err := h.Auth.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	out := make([]apiv1.Session, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, apiv1.Session{
			ID:        s.TokenHash,
			UserId:    uuid.MustParse(s.UserID),
			Username:  s.Username,
			CreatedAt: s.CreatedAt,
			ExpiresAt: s.ExpiresAt,
		})
	}
	return &apiv1.ListSessionsOK{Sessions: out}, nil
}

func (h *Handler) RevokeSession(ctx context.Context, params apiv1.RevokeSessionParams) error {
	if h.Auth == nil {
		return errAuthNotConfigured()
	}
	if err := h.Auth.RevokeSession(ctx, params.SessionId); err != nil {
		return mapAuthError(err)
	}
	return nil
}
