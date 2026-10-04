package api

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// smbRefreshTimeout bounds the smb.conf regeneration that follows a
// grant change, which runs detached from the request: the change is
// already stored by then.
const smbRefreshTimeout = 30 * time.Second

// refreshSMBAccess regenerates smb.conf after a change to who may reach
// which share (D4). Without a share service there is no smb.conf to
// regenerate.
func (h *Handler) refreshSMBAccess(ctx context.Context) error {
	if h.Shares == nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), smbRefreshTimeout)
	defer cancel()
	if err := h.Shares.RefreshAccess(rctx); err != nil {
		log.Printf("api: regenerating smb.conf after an access change: %v", err)
		return &apiError{code: "smb_regeneration_failed", statusCode: http.StatusInternalServerError, message: "the access change was saved, but smb.conf could not be regenerated, so Samba still enforces the previous access"}
	}
	return nil
}

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
	if err := h.refreshSMBAccess(ctx); err != nil {
		return nil, err
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
