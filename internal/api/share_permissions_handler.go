package api

import (
	"context"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func sharePermissionUsersToAPI(entries []SharePermissionUser) []apiv1.UserPermissionEntry {
	out := make([]apiv1.UserPermissionEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, apiv1.UserPermissionEntry{
			UserId:   uuid.MustParse(e.UserID),
			Username: e.Username,
			Access:   apiv1.ShareAccessLevel(e.Access),
		})
	}
	return out
}

func sharePermissionGroupsToAPI(entries []SharePermissionGroup) []apiv1.GroupPermissionEntry {
	out := make([]apiv1.GroupPermissionEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, apiv1.GroupPermissionEntry{
			GroupId: uuid.MustParse(e.GroupID),
			Name:    e.Name,
			Access:  apiv1.ShareAccessLevel(e.Access),
		})
	}
	return out
}

func (h *Handler) GetSharePermissions(ctx context.Context, params apiv1.GetSharePermissionsParams) (*apiv1.SharePermissionsResult, error) {
	if h.Shares == nil {
		return nil, errSharesNotConfigured()
	}
	if h.Auth == nil {
		return nil, errAuthNotConfigured()
	}
	name := string(params.Name)
	if _, err := h.Shares.Get(ctx, name); err != nil {
		return nil, mapShareError(err)
	}
	users, groups, err := h.Auth.GetSharePermissions(ctx, name)
	if err != nil {
		return nil, mapAuthError(err)
	}
	return &apiv1.SharePermissionsResult{
		Users:  sharePermissionUsersToAPI(users),
		Groups: sharePermissionGroupsToAPI(groups),
	}, nil
}

func (h *Handler) UpdateSharePermissions(ctx context.Context, req *apiv1.UpdateSharePermissionsRequest, params apiv1.UpdateSharePermissionsParams) (*apiv1.SharePermissionsResult, error) {
	if h.Shares == nil {
		return nil, errSharesNotConfigured()
	}
	if h.Auth == nil {
		return nil, errAuthNotConfigured()
	}
	name := string(params.Name)
	if _, err := h.Shares.Get(ctx, name); err != nil {
		return nil, mapShareError(err)
	}

	users := make([]PermissionGrant, 0, len(req.Users))
	for _, u := range req.Users {
		users = append(users, PermissionGrant{ID: u.UserId.String(), Access: string(u.Access)})
	}
	groups := make([]PermissionGrant, 0, len(req.Groups))
	for _, g := range req.Groups {
		groups = append(groups, PermissionGrant{ID: g.GroupId.String(), Access: string(g.Access)})
	}
	if err := h.Auth.SetSharePermissions(ctx, name, users, groups); err != nil {
		return nil, mapAuthError(err)
	}

	updatedUsers, updatedGroups, err := h.Auth.GetSharePermissions(ctx, name)
	if err != nil {
		return nil, mapAuthError(err)
	}
	return &apiv1.SharePermissionsResult{
		Users:  sharePermissionUsersToAPI(updatedUsers),
		Groups: sharePermissionGroupsToAPI(updatedGroups),
	}, nil
}
