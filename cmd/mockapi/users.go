package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
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

// errUserExists mirrors internal/api's ErrUserExists (users_admin.go):
// CreateUser refuses a duplicate username, case-insensitively.
func errUserExists(username string) error {
	return &mockError{code: "user_exists", statusCode: 409, message: fmt.Sprintf("a user named %q already exists", username)}
}

func errGroupNotFound(id uuid.UUID) error {
	return &mockError{code: "group_not_found", statusCode: 404, message: fmt.Sprintf("no group with id %s", id)}
}

func errSessionNotFound(id string) error {
	return &mockError{code: "session_not_found", statusCode: 404, message: fmt.Sprintf("no session with id %s", id)}
}

func errApiTokenNotFound(id string) error {
	return &mockError{code: "api_token_not_found", statusCode: 404, message: fmt.Sprintf("no api token with id %s", id)}
}

func errUserNotFoundByUsername(username string) error {
	return &mockError{code: "user_not_found", statusCode: 404, message: fmt.Sprintf("no user %q", username)}
}

// errDuplicateGrant mirrors internal/api's own ErrDuplicateGrant
// (share_permissions.go), which groups_store.go's SetGroupMembers
// returns as duplicate_grant/400 for a repeated id in a full-replace
// write.
func errDuplicateGrant(id uuid.UUID) error {
	return &mockError{code: "duplicate_grant", statusCode: 400, message: fmt.Sprintf("the same id appears more than once in this request: %s", id)}
}

// errShareOnlyNoAPIToken mirrors internal/api's ErrShareOnlyNoAPIToken
// (Q27, doc 01 §5): a share-only account has no API access at all.
func errShareOnlyNoAPIToken() error {
	return &mockError{code: "share_only_no_api_token", statusCode: 403, message: "this account has SMB/NFS access only — it has no API access"}
}

// errTokenRoleExceedsAccount mirrors internal/api's
// ErrTokenRoleExceedsAccount: a token can only narrow an account's own
// access, never widen it.
func errTokenRoleExceedsAccount() error {
	return &mockError{code: "token_role_exceeds_account", statusCode: 403, message: "a token's role cannot exceed its account's own role"}
}

// findUserByUsername is #50's own lookup: createApiToken is keyed by
// username in the path (like the root-only recovery operations), not by
// id, so the mock needs the reverse of the uuid-keyed h.users map.
func (h *handler) findUserByUsername(username string) (apiv1.UserSummary, bool) {
	for _, u := range h.users {
		if u.Username == username {
			return u, true
		}
	}
	return apiv1.UserSummary{}, false
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
	for _, u := range h.users {
		if strings.EqualFold(u.Username, req.Username) {
			return nil, errUserExists(req.Username)
		}
	}
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
	// mirrors internal/api's own AuthStore.SetGroupMembers
	// (groups_store.go): every member id in a full-replace write must
	// already be a real user, and no id may repeat — checked per
	// element, duplicate before unknown, in production's own order.
	seen := make(map[uuid.UUID]struct{}, len(req.UserIds))
	for _, id := range req.UserIds {
		if _, dup := seen[id]; dup {
			return nil, errDuplicateGrant(id)
		}
		seen[id] = struct{}{}
		if _, ok := h.users[id]; !ok {
			return nil, errUserNotFound(id)
		}
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

func (h *handler) CreateApiToken(ctx context.Context, req *apiv1.CreateApiTokenRequest, params apiv1.CreateApiTokenParams) (*apiv1.ApiTokenCreated, error) {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	u, ok := h.findUserByUsername(params.Username)
	if !ok {
		return nil, errUserNotFoundByUsername(params.Username)
	}
	if u.Role == apiv1.UserRoleShareOnly {
		return nil, errShareOnlyNoAPIToken()
	}
	// A token's role can only narrow an account's own role, never widen
	// it: an admin account can be issued a viewer-scoped token, a viewer
	// account can never be issued an admin-scoped one (Q43).
	if req.Role == apiv1.ApiTokenRoleAdmin && u.Role != apiv1.UserRoleAdmin {
		return nil, errTokenRoleExceedsAccount()
	}
	id := uuid.NewString()
	summary := apiv1.ApiTokenSummary{
		ID:        id,
		UserId:    u.ID,
		Username:  u.Username,
		Name:      req.Name,
		Role:      req.Role,
		CreatedAt: time.Now().UTC(),
	}
	h.apiTokens[id] = summary
	return &apiv1.ApiTokenCreated{
		ID:        summary.ID,
		UserId:    summary.UserId,
		Username:  summary.Username,
		Name:      summary.Name,
		Role:      summary.Role,
		CreatedAt: summary.CreatedAt,
		Token:     "hspat_mock_" + id,
	}, nil
}

func (h *handler) ListApiTokens(ctx context.Context) (*apiv1.ListApiTokensOK, error) {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	out := make([]apiv1.ApiTokenSummary, 0, len(h.apiTokens))
	for _, t := range h.apiTokens {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return &apiv1.ListApiTokensOK{Tokens: out}, nil
}

func (h *handler) RevokeApiToken(ctx context.Context, params apiv1.RevokeApiTokenParams) error {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	if _, ok := h.apiTokens[params.TokenId]; !ok {
		return errApiTokenNotFound(params.TokenId)
	}
	delete(h.apiTokens, params.TokenId)
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

	// mirrors internal/api's own SetSharePermissions (share_permissions.go):
	// a repeated user or group id is refused with duplicate_grant before
	// any existence check (rejectDuplicateGrants), then every id must
	// already exist (existsInTx) — an unknown id is refused with
	// user_not_found/group_not_found, not written with an empty username.
	seenUsers := make(map[uuid.UUID]struct{}, len(req.Users))
	for _, u := range req.Users {
		if _, dup := seenUsers[u.UserId]; dup {
			return nil, errDuplicateGrant(u.UserId)
		}
		seenUsers[u.UserId] = struct{}{}
	}
	seenGroups := make(map[uuid.UUID]struct{}, len(req.Groups))
	for _, g := range req.Groups {
		if _, dup := seenGroups[g.GroupId]; dup {
			return nil, errDuplicateGrant(g.GroupId)
		}
		seenGroups[g.GroupId] = struct{}{}
	}
	users := make([]apiv1.UserPermissionEntry, 0, len(req.Users))
	for _, u := range req.Users {
		account, ok := h.users[u.UserId]
		if !ok {
			return nil, errUserNotFound(u.UserId)
		}
		users = append(users, apiv1.UserPermissionEntry{UserId: u.UserId, Username: account.Username, Access: u.Access})
	}
	groups := make([]apiv1.GroupPermissionEntry, 0, len(req.Groups))
	for _, g := range req.Groups {
		group, ok := h.userGroups[g.GroupId]
		if !ok {
			return nil, errGroupNotFound(g.GroupId)
		}
		groups = append(groups, apiv1.GroupPermissionEntry{GroupId: g.GroupId, Name: group.Name, Access: g.Access})
	}
	result := apiv1.SharePermissionsResult{Users: users, Groups: groups}
	h.sharePermissions[params.Name] = result
	return &result, nil
}
