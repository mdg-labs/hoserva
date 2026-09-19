package api_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/notify"
)

func withPeerCred(ctx context.Context, cred auth.PeerCredential) context.Context {
	return auth.WithPeerCredential(ctx, cred)
}

func newRecoveryHandler(t *testing.T) (*api.Handler, *api.AuthService) {
	h, svc, _ := newRecoveryHandlerWithDB(t)
	return h, svc
}

func newRecoveryHandlerWithDB(t *testing.T) (*api.Handler, *api.AuthService, *sql.DB) {
	svc, db := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	return &api.Handler{Auth: svc}, svc, db
}

func newRecoveryHandlerWithNotify(t *testing.T) (*api.Handler, *api.AuthService, *sql.DB) {
	h, svc, db := newRecoveryHandlerWithDB(t)
	notifySvc := notify.NewService(notify.NewStore(db), notify.FakeSecretCipher{}, nil)
	ctx := context.Background()
	if _, err := notifySvc.CreateChannel(ctx, notify.ChannelInput{
		Name: "Ops webhook", Type: notify.ChannelWebhook, Enabled: true,
		Config: notify.ChannelConfig{WebhookURL: "https://example.com/hook"},
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	h.Notify = notifySvc
	return h, svc, db
}

func TestUnlockUserRequiresRootUID(t *testing.T) {
	h, _ := newRecoveryHandler(t)
	ctx := withPeerCred(context.Background(), auth.PeerCredential{UID: 0, GID: 0})

	if err := h.UnlockUser(ctx, apiv1.UnlockUserParams{Username: "admin"}); err != nil {
		t.Fatalf("UnlockUser as root: %v", err)
	}

	ctxDaemon := withPeerCred(context.Background(), auth.PeerCredential{UID: 4242, GID: 4242})
	if err := h.UnlockUser(ctxDaemon, apiv1.UnlockUserParams{Username: "admin"}); !errors.Is(err, api.ErrRootOnlyRecovery) {
		t.Fatalf("daemon uid UnlockUser = %v, want ErrRootOnlyRecovery", err)
	}

	if err := h.UnlockUser(context.Background(), apiv1.UnlockUserParams{Username: "admin"}); !errors.Is(err, api.ErrRootOnlyRecovery) {
		t.Fatalf("no peer cred UnlockUser = %v, want ErrRootOnlyRecovery", err)
	}
}

func TestUnlockUserClearsLockoutAndLoginSucceeds(t *testing.T) {
	h, svc := newRecoveryHandler(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		_, _, _ = svc.Login(ctx, "admin", "wrong-password", "", "203.0.113.4")
	}
	_, _, err := svc.Login(ctx, "admin", "correct horse battery staple", "", "203.0.113.4")
	if err == nil {
		t.Fatal("expected lockout before unlock")
	}

	rootCtx := withPeerCred(ctx, auth.PeerCredential{UID: 0, GID: 0})
	if err := h.UnlockUser(rootCtx, apiv1.UnlockUserParams{Username: "admin"}); err != nil {
		t.Fatalf("UnlockUser: %v", err)
	}

	// Use a fresh source address — unlock clears the account bucket only (Q78).
	_, _, err = svc.Login(ctx, "admin", "correct horse battery staple", "", "203.0.113.5")
	if err != nil {
		t.Fatalf("login after unlock: %v", err)
	}
}

func TestRecoveryRefusedWithoutPeerCred(t *testing.T) {
	h, _ := newRecoveryHandler(t)
	if err := h.UnlockUser(context.Background(), apiv1.UnlockUserParams{Username: "admin"}); !errors.Is(err, api.ErrRootOnlyRecovery) {
		t.Fatalf("UnlockUser without peer cred = %v, want ErrRootOnlyRecovery", err)
	}
}

func TestResetPasswordRequiresRootUID(t *testing.T) {
	h, _ := newRecoveryHandler(t)
	req := &apiv1.ResetUserPasswordRequest{Password: "new-password-12chars"}
	ctx := withPeerCred(context.Background(), auth.PeerCredential{UID: 1000, GID: 1000})
	if err := h.ResetUserPassword(ctx, req, apiv1.ResetUserPasswordParams{Username: "admin"}); !errors.Is(err, api.ErrRootOnlyRecovery) {
		t.Fatalf("non-root ResetUserPassword = %v, want ErrRootOnlyRecovery", err)
	}
}

func TestDisableTotpRequiresRootUID(t *testing.T) {
	h, _ := newRecoveryHandler(t)
	ctx := withPeerCred(context.Background(), auth.PeerCredential{UID: 1000, GID: 1000})
	if err := h.DisableUserTotp(ctx, apiv1.DisableUserTotpParams{Username: "admin"}); !errors.Is(err, api.ErrRootOnlyRecovery) {
		t.Fatalf("non-root DisableUserTotp = %v, want ErrRootOnlyRecovery", err)
	}
}

func TestRecoveryRollbackOnAuditFailure(t *testing.T) {
	h, _, db := newRecoveryHandlerWithNotify(t)
	ctx := withPeerCred(context.Background(), auth.PeerCredential{UID: 0, GID: 0})

	var originalHash string
	if err := db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE username = ?`, "admin").Scan(&originalHash); err != nil {
		t.Fatalf("reading original password hash: %v", err)
	}

	api.SetRecoveryAuditTestHook(func() error { return fmt.Errorf("forced audit failure") })
	defer api.SetRecoveryAuditTestHook(nil)

	req := &apiv1.ResetUserPasswordRequest{Password: "new-password-12chars"}
	if err := h.ResetUserPassword(ctx, req, apiv1.ResetUserPasswordParams{Username: "admin"}); err == nil {
		t.Fatal("ResetUserPassword: expected error from forced audit failure")
	}

	var currentHash string
	if err := db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE username = ?`, "admin").Scan(&currentHash); err != nil {
		t.Fatalf("reading password hash after rollback: %v", err)
	}
	if currentHash != originalHash {
		t.Fatal("password hash changed despite rolled-back recovery transaction")
	}

	var auditCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&auditCount); err != nil {
		t.Fatalf("counting audit_log: %v", err)
	}
	if auditCount != 0 {
		t.Fatalf("audit_log count = %d, want 0 after rollback", auditCount)
	}

	var deliveryCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_deliveries`).Scan(&deliveryCount); err != nil {
		t.Fatalf("counting notify_deliveries: %v", err)
	}
	if deliveryCount != 0 {
		t.Fatalf("notify_deliveries count = %d, want 0 after rollback", deliveryCount)
	}
}
