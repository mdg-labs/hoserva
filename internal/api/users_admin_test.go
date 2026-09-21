package api_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/share"
)

func TestCreateUserDefaultsToShareOnly(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	u, err := svc.CreateUser(ctx, "kid", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.Role != "share-only" {
		t.Errorf("role = %q, want share-only (Q27's default for a new account)", u.Role)
	}
}

func TestCreateUserRejectsAdminRole(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	if _, err := svc.CreateUser(ctx, "second-admin", "admin"); !errors.Is(err, api.ErrInvalidRole) {
		t.Errorf("CreateUser(role=admin) = %v, want ErrInvalidRole (single admin is created only by createFirstAdmin)", err)
	}
}

func TestCreateUserRejectsDuplicateUsername(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	if _, err := svc.CreateUser(ctx, "kid", "viewer"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := svc.CreateUser(ctx, "Kid", ""); !errors.Is(err, api.ErrUserExists) {
		t.Errorf("CreateUser(duplicate, case-insensitive) = %v, want ErrUserExists", err)
	}
}

// TestShareOnlyAccountHasNoUILogin is #49's first acceptance criterion:
// a share-only account's credentials are real and correct, but Login
// still refuses it — "no UI login" is enforced at login itself, not left
// to fall out of every later operation's own role check.
func TestShareOnlyAccountHasNoUILogin(t *testing.T) {
	svc, _ := newAuthTestService(t)
	svc.SambaAccounts = share.NewFakeSambaAccounts()
	ctx := context.Background()

	u, err := svc.CreateUser(ctx, "kid", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := svc.SetUserPassword(ctx, u.ID, "correct horse battery staple"); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}

	_, _, err = svc.Login(ctx, "kid", "correct horse battery staple", "", "")
	if !errors.Is(err, api.ErrShareOnlyNoLogin) {
		t.Errorf("Login(share-only account, correct password) = %v, want ErrShareOnlyNoLogin", err)
	}
}

func TestUpdateUserRoleRefusesAdminAccount(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	admin, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	if _, err := svc.UpdateUserRole(ctx, admin.ID, "viewer"); !errors.Is(err, api.ErrCannotModifyAdmin) {
		t.Errorf("UpdateUserRole(admin) = %v, want ErrCannotModifyAdmin", err)
	}
}

func TestUpdateUserRoleChangesViewerToShareOnly(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, "kid", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	updated, err := svc.UpdateUserRole(ctx, u.ID, "share-only")
	if err != nil {
		t.Fatalf("UpdateUserRole: %v", err)
	}
	if updated.Role != "share-only" {
		t.Errorf("role = %q, want share-only", updated.Role)
	}
}

func TestDeleteUserRefusesAdminAccount(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	admin, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	if err := svc.DeleteUser(ctx, admin.ID); !errors.Is(err, api.ErrCannotModifyAdmin) {
		t.Errorf("DeleteUser(admin) = %v, want ErrCannotModifyAdmin", err)
	}
}

// TestDeleteUserRemovesSessionsAndGroupMembership proves DeleteUser's
// cascade actually runs, not only that the users row disappears: a
// dangling sessions or user_group_members row referencing a deleted user
// id would be silently invisible to every other test, since store.DSN's
// runtime connections don't enforce foreign keys.
func TestDeleteUserRemovesSessionsAndGroupMembership(t *testing.T) {
	svc, _ := newAuthTestService(t)
	svc.SambaAccounts = share.NewFakeSambaAccounts()
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, "kid", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := svc.SetUserPassword(ctx, u.ID, "correct horse battery staple"); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}
	if _, _, err := svc.Login(ctx, "kid", "correct horse battery staple", "", ""); err != nil {
		t.Fatalf("Login: %v", err)
	}
	g, err := svc.CreateGroup(ctx, "family")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if _, err := svc.SetGroupMembers(ctx, g.ID, []string{u.ID}); err != nil {
		t.Fatalf("SetGroupMembers: %v", err)
	}

	if err := svc.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	sessions, err := svc.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, s := range sessions {
		if s.UserID == u.ID {
			t.Errorf("session %s still references deleted user %s", s.TokenHash, u.ID)
		}
	}
	updatedGroup, err := svc.SetGroupMembers(ctx, g.ID, nil)
	if err != nil {
		t.Fatalf("SetGroupMembers (read back): %v", err)
	}
	if len(updatedGroup.MemberIDs) != 0 {
		t.Errorf("group members after deleting the user = %v, want none left over from the deleted membership", updatedGroup.MemberIDs)
	}
}

// TestSetUserPasswordRefusesAdminAccount is the fix for the "SetUserPassword
// bypasses Q78's uid-0 recovery gate" finding: an ordinary admin-API caller
// must not be able to change the sole admin's password through this path —
// only the root-only `hoserva user reset-password` command may. This test
// fails against a version of SetUserPassword with no admin-role guard.
func TestSetUserPasswordRefusesAdminAccount(t *testing.T) {
	svc, _ := newAuthTestService(t)
	fake := share.NewFakeSambaAccounts()
	svc.SambaAccounts = fake
	ctx := context.Background()
	admin, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	if _, err := svc.SetUserPassword(ctx, admin.ID, "a brand new password"); !errors.Is(err, api.ErrCannotModifyAdmin) {
		t.Errorf("SetUserPassword(admin) = %v, want ErrCannotModifyAdmin", err)
	}

	stored, err := svc.Store.GetUserByID(ctx, admin.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if !auth.VerifyPassword(stored.PasswordHash, "correct horse battery staple") {
		t.Error("admin password hash changed even though SetUserPassword should have refused")
	}
	if _, ok := fake.Password("admin"); ok {
		t.Error("a Samba account was provisioned for the admin despite SetUserPassword refusing")
	}
}

// TestSetUserPasswordWritesBothCredentialsTogether is #49's second
// acceptance criterion's success path: one call updates the UI hash and
// provisions the Samba account.
func TestSetUserPasswordWritesBothCredentialsTogether(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	fake := share.NewFakeSambaAccounts()
	svc.SambaAccounts = fake

	u, err := svc.CreateUser(ctx, "kid", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := svc.SetUserPassword(ctx, u.ID, "correct horse battery staple"); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}

	stored, err := svc.Store.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if !auth.VerifyPassword(stored.PasswordHash, "correct horse battery staple") {
		t.Error("UI credential hash does not match the password just set")
	}

	pw, ok := fake.Password("kid")
	if !ok {
		t.Fatal("SambaAccounts.SetPassword was never called")
	}
	if pw != "correct horse battery staple" {
		t.Errorf("Samba password recorded = %q, want the same password as the UI credential", pw)
	}
}

// TestSetUserPasswordRollsBackUICredentialOnSambaFailure is the
// safety-critical scenario this issue's acceptance criteria name
// directly: "rolled back together on failure" — if the Samba passdb
// write fails, the UI credential must not end up disagreeing with it.
// This test is written to fail against a version of SetUserPassword that
// writes the UI hash and never rolls it back on a Samba failure.
func TestSetUserPasswordRollsBackUICredentialOnSambaFailure(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	fake := share.NewFakeSambaAccounts()
	svc.SambaAccounts = fake

	u, err := svc.CreateUser(ctx, "kid", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	originalHash, err := svc.Store.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}

	fake.FailSetPassword(errors.New("smbpasswd: simulated failure"))
	_, err = svc.SetUserPassword(ctx, u.ID, "correct horse battery staple")
	if !errors.Is(err, api.ErrSambaPasswordFailed) {
		t.Fatalf("SetUserPassword with a failing Samba write = %v, want ErrSambaPasswordFailed", err)
	}

	stored, err := svc.Store.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if stored.PasswordHash != originalHash.PasswordHash {
		t.Error("UI credential hash changed even though the Samba write failed — the two credentials are now out of sync")
	}
	if _, ok := fake.Password("kid"); ok {
		t.Error("Samba account was recorded despite SetPassword being scripted to fail")
	}

	// The new password must not verify against the rolled-back hash —
	// proving the rollback restored the exact previous value, not merely
	// "some" hash.
	if auth.VerifyPassword(stored.PasswordHash, "correct horse battery staple") {
		t.Error("the new password verifies against the stored hash — the rollback did not actually happen")
	}
}

// TestDeleteUserRemovesSambaAccount is the fix for "deleting a user leaves
// their Samba passdb entry active": a user whose password was set (and so
// has a provisioned Samba account) must have that account removed as part
// of DeleteUser, not just their database row. This test fails against a
// version of DeleteUser that never calls SambaAccounts.Delete.
func TestDeleteUserRemovesSambaAccount(t *testing.T) {
	svc, _ := newAuthTestService(t)
	fake := share.NewFakeSambaAccounts()
	svc.SambaAccounts = fake
	ctx := context.Background()

	u, err := svc.CreateUser(ctx, "bob", "share-only")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := svc.SetUserPassword(ctx, u.ID, "correct horse battery staple"); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}
	if _, ok := fake.Password("bob"); !ok {
		t.Fatal("SambaAccounts.SetPassword was never called — test setup is broken")
	}

	if err := svc.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	if _, ok := fake.Password("bob"); ok {
		t.Error("Samba account for bob still present after DeleteUser — SambaAccounts.Delete was not called")
	}
	if _, err := svc.Store.GetUserByID(ctx, u.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetUserByID(deleted user) = %v, want sql.ErrNoRows", err)
	}
}

// TestDeleteUserNeverCalledSambaDeleteForUnprovisionedAccount proves the
// "safe no-op" half of the same fix: a user who never had a password set
// never provisioned a Samba account, and deleting them must not surface a
// spurious Samba failure.
func TestDeleteUserNeverCalledSambaDeleteForUnprovisionedAccount(t *testing.T) {
	svc, _ := newAuthTestService(t)
	fake := share.NewFakeSambaAccounts()
	svc.SambaAccounts = fake
	ctx := context.Background()

	u, err := svc.CreateUser(ctx, "kid", "share-only")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := svc.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser(never had a password set) = %v, want nil", err)
	}
}

// TestDeleteUserFailsWhollyWhenSambaDeleteFails is the delete-fails-whole-
// operation-fails policy the issue calls for: a failing Samba removal must
// leave the user row, and every row it cascades to, untouched — never a
// half-deleted user (DB row gone, Samba account orphaned) or the reverse.
// This test fails against a version of DeleteUser that removes the
// database row regardless of what SambaAccounts.Delete returns.
func TestDeleteUserFailsWhollyWhenSambaDeleteFails(t *testing.T) {
	svc, _ := newAuthTestService(t)
	fake := share.NewFakeSambaAccounts()
	svc.SambaAccounts = fake
	ctx := context.Background()

	u, err := svc.CreateUser(ctx, "bob", "share-only")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := svc.SetUserPassword(ctx, u.ID, "correct horse battery staple"); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}

	fake.FailDelete(errors.New("smbpasswd: simulated failure"))
	err = svc.DeleteUser(ctx, u.ID)
	if !errors.Is(err, api.ErrSambaDeleteFailed) {
		t.Fatalf("DeleteUser with a failing Samba delete = %v, want ErrSambaDeleteFailed", err)
	}

	if _, err := svc.Store.GetUserByID(ctx, u.ID); err != nil {
		t.Errorf("GetUserByID after a failed delete = %v, want the user row still present", err)
	}
	if _, ok := fake.Password("bob"); !ok {
		t.Error("Samba account for bob is gone even though the delete failed and should have rolled back")
	}
}
