package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// #49's users/groups/sessions/permissions mock: an in-memory store, like
// shares.go's — no real hashing, no real Samba passdb, just enough state
// for a client to exercise the generated operations against.

func errUserNotFound(id uuid.UUID) error {
	return &mockError{code: "user_not_found", statusCode: 404, message: fmt.Sprintf("no user with id %s", id)}
}

func errGroupNotFound(id uuid.UUID) error {
	return &mockError{code: "group_not_found", statusCode: 404, message: fmt.Sprintf("no group with id %s", id)}
}

func errSessionNotFound(id string) error {
	return &mockError{code: "session_not_found", statusCode: 404, message: fmt.Sprintf("no session with id %s", id)}
}

func (h *handler) ListUsers(ctx context.Context) (*apiv1.ListUsersOK, error) {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	out := make([]apiv1.UserSummary, 0, len(h.users))
	for _, u := range h.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return &apiv1.ListUsersOK{Users: out}, nil
}

func (h *handler) CreateUser(ctx context.Context, req *apiv1.CreateUserRequest) (*apiv1.UserSummary, error) {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	role := apiv1.UserRoleShareOnly
	if v, ok := req.Role.Get(); ok {
		role = apiv1.UserRole(v)
	}
	u := apiv1.UserSummary{
		ID:           uuid.New(),
		Username:     req.Username,
		Role:         role,
		TotpEnrolled: false,
		CreatedAt:    time.Now().UTC(),
	}
	h.users[u.ID] = u
	return &u, nil
}

func (h *handler) UpdateUser(ctx context.Context, req *apiv1.UpdateUserRequest, params apiv1.UpdateUserParams) (*apiv1.UserSummary, error) {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	u, ok := h.users[params.UserId]
	if !ok {
		return nil, errUserNotFound(params.UserId)
	}
	u.Role = apiv1.UserRole(req.Role)
	h.users[params.UserId] = u
	return &u, nil
}

func (h *handler) DeleteUser(ctx context.Context, params apiv1.DeleteUserParams) error {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	if _, ok := h.users[params.UserId]; !ok {
		return errUserNotFound(params.UserId)
	}
	delete(h.users, params.UserId)
	delete(h.userSharePermissions, params.UserId)
	return nil
}

func (h *handler) SetUserPassword(ctx context.Context, req *apiv1.SetUserPasswordRequest, params apiv1.SetUserPasswordParams) error {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	if _, ok := h.users[params.UserId]; !ok {
		return errUserNotFound(params.UserId)
	}
	return nil
}

func (h *handler) GetUserSharePermissions(ctx context.Context, params apiv1.GetUserSharePermissionsParams) (*apiv1.UserSharePermissionsResult, error) {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	if _, ok := h.users[params.UserId]; !ok {
		return nil, errUserNotFound(params.UserId)
	}
	result, ok := h.userSharePermissions[params.UserId]
	if !ok {
		result = apiv1.UserSharePermissionsResult{Permissions: []apiv1.UserSharePermission{}}
	}
	return &result, nil
}

func (h *handler) UpdateUserSharePermissions(ctx context.Context, req *apiv1.UpdateUserSharePermissionsRequest, params apiv1.UpdateUserSharePermissionsParams) (*apiv1.UserSharePermissionsResult, error) {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	if _, ok := h.users[params.UserId]; !ok {
		return nil, errUserNotFound(params.UserId)
	}
	permissions := req.Permissions
	if permissions == nil {
		permissions = []apiv1.UserSharePermission{}
	}
	result := apiv1.UserSharePermissionsResult{Permissions: permissions}
	h.userSharePermissions[params.UserId] = result
	return &result, nil
}

func (h *handler) ListUserGroups(ctx context.Context) (*apiv1.ListUserGroupsOK, error) {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	out := make([]apiv1.UserGroup, 0, len(h.userGroups))
	for _, g := range h.userGroups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return &apiv1.ListUserGroupsOK{Groups: out}, nil
}

func (h *handler) CreateUserGroup(ctx context.Context, req *apiv1.CreateUserGroupRequest) (*apiv1.UserGroup, error) {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	g := apiv1.UserGroup{ID: uuid.New(), Name: req.Name, MemberUserIds: []uuid.UUID{}}
	h.userGroups[g.ID] = g
	return &g, nil
}

func (h *handler) DeleteUserGroup(ctx context.Context, params apiv1.DeleteUserGroupParams) error {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	if _, ok := h.userGroups[params.GroupId]; !ok {
		return errGroupNotFound(params.GroupId)
	}
	delete(h.userGroups, params.GroupId)
	return nil
}

func (h *handler) SetUserGroupMembers(ctx context.Context, req *apiv1.SetUserGroupMembersRequest, params apiv1.SetUserGroupMembersParams) (*apiv1.UserGroup, error) {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	g, ok := h.userGroups[params.GroupId]
	if !ok {
		return nil, errGroupNotFound(params.GroupId)
	}
	members := req.UserIds
	if members == nil {
		members = []uuid.UUID{}
	}
	g.MemberUserIds = members
	h.userGroups[params.GroupId] = g
	return &g, nil
}

func (h *handler) ListSessions(ctx context.Context) (*apiv1.ListSessionsOK, error) {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	out := make([]apiv1.Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return &apiv1.ListSessionsOK{Sessions: out}, nil
}

func (h *handler) RevokeSession(ctx context.Context, params apiv1.RevokeSessionParams) error {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	if _, ok := h.sessions[params.SessionId]; !ok {
		return errSessionNotFound(params.SessionId)
	}
	delete(h.sessions, params.SessionId)
	return nil
}

func (h *handler) GetSharePermissions(ctx context.Context, params apiv1.GetSharePermissionsParams) (*apiv1.SharePermissionsResult, error) {
	h.mu.Lock()
	_, ok := h.shares[string(params.Name)]
	h.mu.Unlock()
	if !ok {
		return nil, errShareNotFound(params.Name)
	}
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	result, ok := h.sharePermissions[params.Name]
	if !ok {
		result = apiv1.SharePermissionsResult{Users: []apiv1.UserPermissionEntry{}, Groups: []apiv1.GroupPermissionEntry{}}
	}
	return &result, nil
}

func (h *handler) UpdateSharePermissions(ctx context.Context, req *apiv1.UpdateSharePermissionsRequest, params apiv1.UpdateSharePermissionsParams) (*apiv1.SharePermissionsResult, error) {
	h.mu.Lock()
	_, ok := h.shares[string(params.Name)]
	h.mu.Unlock()
	if !ok {
		return nil, errShareNotFound(params.Name)
	}
	h.usersMu.Lock()
	defer h.usersMu.Unlock()

	users := make([]apiv1.UserPermissionEntry, 0, len(req.Users))
	for _, u := range req.Users {
		username := ""
		if account, ok := h.users[u.UserId]; ok {
			username = account.Username
		}
		users = append(users, apiv1.UserPermissionEntry{UserId: u.UserId, Username: username, Access: u.Access})
	}
	groups := make([]apiv1.GroupPermissionEntry, 0, len(req.Groups))
	for _, g := range req.Groups {
		name := ""
		if group, ok := h.userGroups[g.GroupId]; ok {
			name = group.Name
		}
		groups = append(groups, apiv1.GroupPermissionEntry{GroupId: g.GroupId, Name: name, Access: g.Access})
	}
	result := apiv1.SharePermissionsResult{Users: users, Groups: groups}
	h.sharePermissions[params.Name] = result
	return &result, nil
}
