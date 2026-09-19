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
	apiv1.CancelJobOperation:                 RoleAdmin,
	apiv1.ConfirmTotpOperation:               RoleViewer,
	apiv1.CreateArrayOperation:               RoleAdmin,
	apiv1.CreateFirstAdminOperation:          RolePublic,
	apiv1.CreateNotificationChannelOperation: RoleAdmin,
	apiv1.DeleteNotificationChannelOperation: RoleAdmin,
	apiv1.DisableUserTotpOperation:           RoleAdmin,
	apiv1.EnrollTotpOperation:                RoleViewer,
	apiv1.ExportConfigOperation:              RoleAdmin,
	apiv1.GetCurrentSessionOperation:         RoleViewer,
	apiv1.GetGeneralSettingsOperation:        RoleViewer,
	apiv1.GetJobOperation:                    RoleViewer,
	apiv1.GetJobLogOperation:                 RoleViewer,
	apiv1.GetNotificationChannelOperation:    RoleViewer,
	apiv1.GetNotificationRoutingOperation:    RoleViewer,
	apiv1.GetPoolOperation:                   RoleViewer,
	apiv1.GetQuietHoursOperation:             RoleViewer,
	apiv1.GetSetupStatusOperation:            RolePublic,
	apiv1.GetStatusOperation:                 RoleViewer,
	apiv1.ImportConfigOperation:              RoleAdmin,
	apiv1.ListDisksOperation:                 RoleViewer,
	apiv1.ListJobsOperation:                  RoleViewer,
	apiv1.ListNotificationChannelsOperation:  RoleViewer,
	apiv1.LoginOperation:                     RolePublic,
	apiv1.LogoutOperation:                    RoleViewer,
	apiv1.ResetUserPasswordOperation:         RoleAdmin,
	apiv1.ResumeJobOperation:                 RoleAdmin,
	apiv1.RunDoctorOperation:                 RoleViewer,
	apiv1.SendTestNotificationOperation:      RoleAdmin,
	apiv1.StartArrayOperation:                RoleAdmin,
	apiv1.StartFixOperation:                  RoleAdmin,
	apiv1.StartScrubOperation:                RoleAdmin,
	apiv1.StartSyncOperation:                 RoleAdmin,
	apiv1.StopArrayOperation:                 RoleAdmin,
	apiv1.UnlockUserOperation:                RoleAdmin,
	apiv1.UpdateGeneralSettingsOperation:     RoleAdmin,
	apiv1.UpdateNotificationChannelOperation: RoleAdmin,
	apiv1.UpdateNotificationRouteOperation:   RoleAdmin,
	apiv1.UpdateQuietHoursOperation:          RoleAdmin,
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
