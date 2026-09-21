package api

import (
	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// Role mirrors api/openapi.yaml's x-hoserva-role values (D18).
type Role string

const (
	// RolePublic marks an operation reachable with no credential at all
	// (doc 03 §1's pre-setup routes, plus login itself).
	RolePublic Role = "public"
	RoleViewer Role = "viewer"
	RoleAdmin  Role = "admin"
)

// operationRoles is every generated operation's x-hoserva-role, exactly as
// declared in api/openapi.yaml (D18). roles_test.go parses the spec itself
// and fails the build if this map ever drifts from it — the "generated at
// make gen time, or read from the embedded spec" note in #22's dispatch,
// done as a cross-check instead: api/openapi.yaml is outside internal/api's
// own file scope for this issue, so nothing here can go:embed it directly
// (embed patterns cannot ascend into a parent directory), and Makefile's
// `gen` target is likewise outside this issue's scope to extend — a test
// that reads the spec from the repository (never from a built binary) is
// the mechanism available.
var operationRoles = map[apiv1.OperationName]Role{
	apiv1.CancelJobOperation:                      RoleAdmin,
	apiv1.ConfirmTotpOperation:                    RoleViewer,
	apiv1.CreateApiTokenOperation:                 RoleAdmin,
	apiv1.CreateArrayOperation:                    RoleAdmin,
	apiv1.CreateFirstAdminOperation:               RolePublic,
	apiv1.CreateNotificationChannelOperation:      RoleAdmin,
	apiv1.CreateUserOperation:                     RoleAdmin,
	apiv1.CreateUserGroupOperation:                RoleAdmin,
	apiv1.DeleteNotificationChannelOperation:      RoleAdmin,
	apiv1.DeleteUserOperation:                     RoleAdmin,
	apiv1.DeleteUserGroupOperation:                RoleAdmin,
	apiv1.DisableUserTotpOperation:                RoleAdmin,
	apiv1.EnrollTotpOperation:                     RoleViewer,
	apiv1.ExportConfigOperation:                   RoleAdmin,
	apiv1.GetCurrentSessionOperation:              RoleViewer,
	apiv1.GetGeneralSettingsOperation:             RoleViewer,
	apiv1.GetSchedulesOperation:                   RoleViewer,
	apiv1.GetJobOperation:                         RoleViewer,
	apiv1.GetJobLogOperation:                      RoleViewer,
	apiv1.GetNotificationChannelOperation:         RoleViewer,
	apiv1.GetNotificationRoutingOperation:         RoleViewer,
	apiv1.GetParityOperation:                      RoleViewer,
	apiv1.GetPoolOperation:                        RoleViewer,
	apiv1.GetQuietHoursOperation:                  RoleViewer,
	apiv1.GetSetupStatusOperation:                 RolePublic,
	apiv1.GetStatusOperation:                      RoleViewer,
	apiv1.GetMetricsOperation:                     RoleViewer,
	apiv1.GetSharePermissionsOperation:            RoleViewer,
	apiv1.GetUserSharePermissionsOperation:        RoleViewer,
	apiv1.ImportConfigOperation:                   RoleAdmin,
	apiv1.EjectExternalDiskOperation:              RoleAdmin,
	apiv1.FormatExternalDiskOperation:             RoleAdmin,
	apiv1.ListApiTokensOperation:                  RoleViewer,
	apiv1.ListDisksOperation:                      RoleViewer,
	apiv1.ListExternalDisksOperation:              RoleViewer,
	apiv1.MountExternalDiskOperation:              RoleAdmin,
	apiv1.RegisterExternalDiskOperation:           RoleAdmin,
	apiv1.UpdateExternalDiskOperation:             RoleAdmin,
	apiv1.ListSessionsOperation:                   RoleViewer,
	apiv1.ListUserGroupsOperation:                 RoleViewer,
	apiv1.ListUsersOperation:                      RoleViewer,
	apiv1.ListWakeEventsOperation:                 RoleViewer,
	apiv1.ListJobsOperation:                       RoleViewer,
	apiv1.ListNotificationChannelsOperation:       RoleViewer,
	apiv1.ListNotificationsOperation:              RoleViewer,
	apiv1.LoginOperation:                          RolePublic,
	apiv1.MarkNotificationsReadOperation:          RoleViewer,
	apiv1.LogoutOperation:                         RoleViewer,
	apiv1.ResetUserPasswordOperation:              RoleAdmin,
	apiv1.ResumeJobOperation:                      RoleAdmin,
	apiv1.RevokeApiTokenOperation:                 RoleAdmin,
	apiv1.RevokeSessionOperation:                  RoleAdmin,
	apiv1.RunDoctorOperation:                      RoleViewer,
	apiv1.RunParityDiffOperation:                  RoleAdmin,
	apiv1.SendTestNotificationOperation:           RoleAdmin,
	apiv1.SetUserGroupMembersOperation:            RoleAdmin,
	apiv1.SetUserPasswordOperation:                RoleAdmin,
	apiv1.StartArrayOperation:                     RoleAdmin,
	apiv1.StartFixOperation:                       RoleAdmin,
	apiv1.StartScrubOperation:                     RoleAdmin,
	apiv1.StartSyncOperation:                      RoleAdmin,
	apiv1.StopArrayOperation:                      RoleAdmin,
	apiv1.UnlockUserOperation:                     RoleAdmin,
	apiv1.UpdateGeneralSettingsOperation:          RoleAdmin,
	apiv1.UpdateSharePermissionsOperation:         RoleAdmin,
	apiv1.UpdateUserOperation:                     RoleAdmin,
	apiv1.UpdateUserSharePermissionsOperation:     RoleAdmin,
	apiv1.UpdateMaintenanceChainScheduleOperation: RoleAdmin,
	apiv1.UpdateScheduledJobOperation:             RoleAdmin,
	apiv1.UpdateNotificationChannelOperation:      RoleAdmin,
	apiv1.UpdateNotificationRouteOperation:        RoleAdmin,
	apiv1.UpdateQuietHoursOperation:               RoleAdmin,
	apiv1.GetUpdateStatusOperation:                RoleViewer,
	apiv1.UpdateUpdateSettingsOperation:           RoleAdmin,
	apiv1.CheckForUpdateOperation:                 RoleAdmin,
	apiv1.ApplyHostConfigOperation:                RoleAdmin,
	apiv1.ApplyUpdateOperation:                    RoleAdmin,
	apiv1.RollbackUpdateOperation:                 RoleAdmin,
	apiv1.RebootHostOperation:                     RoleAdmin,
	apiv1.ListSharesOperation:                     RoleViewer,
	apiv1.GetShareOperation:                       RoleViewer,
	apiv1.BrowseShareOperation:                    RoleViewer,
	apiv1.CreateShareOperation:                    RoleAdmin,
	apiv1.UpdateShareOperation:                    RoleAdmin,
	apiv1.DeleteShareOperation:                    RoleAdmin,
	apiv1.DeleteShareDataOperation:                RoleAdmin,
	apiv1.GetNetworkSettingsOperation:             RoleViewer,
	apiv1.ApplyNetworkSettingsOperation:           RoleAdmin,
	apiv1.ConfirmNetworkSettingsOperation:         RoleAdmin,
	apiv1.RegenerateTLSCertificateOperation:       RoleAdmin,
	apiv1.ConfigureLetsEncryptOperation:           RoleAdmin,
	apiv1.DisableLetsEncryptOperation:             RoleAdmin,
}

// RoleFor returns operation's required role, and false if the operation
// has no entry at all — a caller must treat that as a refusal (fail
// closed), never as "no restriction" (doc 01 §5, D18).
func RoleFor(operation apiv1.OperationName) (Role, bool) {
	r, ok := operationRoles[operation]
	return r, ok
}

// Satisfies reports whether a principal with role r may call an operation
// that requires role required: admin satisfies anything; viewer only
// satisfies a viewer requirement. RolePublic is never a principal's own
// role — only ever a requirement, and requirement RolePublic is satisfied
// by any r since a public operation's Handle* method is never invoked at
// all (ogen's security: [] skips it), so Satisfies is not consulted for it
// in practice; it returns true here anyway rather than panicking on an
// input that can't occur.
func (r Role) Satisfies(required Role) bool {
	if r == RoleAdmin || required == RolePublic {
		return true
	}
	return r == required
}

// effectiveTokenRole caps a personal API token's own declared scope
// (Q43, always admin or viewer — CreateAPIToken's validateTokenRole) to
// its owning account's *current* role, so an account demoted after a
// token was issued can never keep using that token at its old privilege
// level — mirroring how a session's role is always read fresh from the
// account (HandleSessionCookie) rather than cached from login time, and
// needing no separate revocation step of its own. accountRole is the
// users.role column's own value (admin/viewer/share-only, Q27); there is
// no special case for share-only below, because Role("share-only") never
// equals RoleAdmin and never satisfies RoleViewer or RoleAdmin, so it
// falls out of Satisfies the same way a share-only session would if
// Login ever let one through (it refuses outright instead,
// ErrShareOnlyNoLogin).
func effectiveTokenRole(accountRole, tokenRole string) Role {
	account := Role(accountRole)
	token := Role(tokenRole)
	if account.Satisfies(token) {
		return token
	}
	return account
}
