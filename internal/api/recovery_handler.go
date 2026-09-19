package api

import (
	"context"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func (h *Handler) ResetUserPassword(ctx context.Context, req *apiv1.ResetUserPasswordRequest, params apiv1.ResetUserPasswordParams) error {
	if err := requireRootPeer(ctx); err != nil {
		return err
	}
	if h.Auth == nil {
		return fmt.Errorf("auth service not configured")
	}
	detail := fmt.Sprintf("user %q password reset", params.Username)
	title := "Account password reset"
	msg := fmt.Sprintf("The password for user %q was reset by root over the Unix socket.", params.Username)
	return h.performRecovery(ctx, func(txStore *AuthStore) (*recoveryOutcome, error) {
		return h.Auth.applyPasswordResetTx(ctx, txStore, params.Username, req.Password)
	}, "user_reset_password", detail, title, msg)
}

func (h *Handler) DisableUserTotp(ctx context.Context, params apiv1.DisableUserTotpParams) error {
	if err := requireRootPeer(ctx); err != nil {
		return err
	}
	if h.Auth == nil {
		return fmt.Errorf("auth service not configured")
	}
	detail := fmt.Sprintf("user %q TOTP disabled", params.Username)
	title := "Account TOTP disabled"
	msg := fmt.Sprintf("TOTP was disabled for user %q by root over the Unix socket.", params.Username)
	return h.performRecovery(ctx, func(txStore *AuthStore) (*recoveryOutcome, error) {
		return h.Auth.applyDisableTotpTx(ctx, txStore, params.Username)
	}, "user_disable_totp", detail, title, msg)
}

func (h *Handler) UnlockUser(ctx context.Context, params apiv1.UnlockUserParams) error {
	if err := requireRootPeer(ctx); err != nil {
		return err
	}
	if h.Auth == nil {
		return fmt.Errorf("auth service not configured")
	}
	detail := fmt.Sprintf("user %q login lockout cleared", params.Username)
	title := "Account lockout cleared"
	msg := fmt.Sprintf("The login lockout for user %q was cleared by root over the Unix socket.", params.Username)
	return h.performRecovery(ctx, func(txStore *AuthStore) (*recoveryOutcome, error) {
		return h.Auth.applyUnlockTx(ctx, txStore, params.Username)
	}, "user_unlock", detail, title, msg)
}
