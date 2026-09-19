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
	if err := h.Auth.ResetUserPassword(ctx, params.Username, req.Password); err != nil {
		return err
	}
	detail := fmt.Sprintf("user %q password reset", params.Username)
	if err := h.auditRecovery(ctx, "user_reset_password", detail); err != nil {
		return err
	}
	title := "Account password reset"
	msg := fmt.Sprintf("The password for user %q was reset by root over the Unix socket.", params.Username)
	if err := h.announceRecovery(ctx, title, msg); err != nil {
		return err
	}
	return nil
}

func (h *Handler) DisableUserTotp(ctx context.Context, params apiv1.DisableUserTotpParams) error {
	if err := requireRootPeer(ctx); err != nil {
		return err
	}
	if h.Auth == nil {
		return fmt.Errorf("auth service not configured")
	}
	if err := h.Auth.DisableUserTotp(ctx, params.Username); err != nil {
		return err
	}
	detail := fmt.Sprintf("user %q TOTP disabled", params.Username)
	if err := h.auditRecovery(ctx, "user_disable_totp", detail); err != nil {
		return err
	}
	title := "Account TOTP disabled"
	msg := fmt.Sprintf("TOTP was disabled for user %q by root over the Unix socket.", params.Username)
	if err := h.announceRecovery(ctx, title, msg); err != nil {
		return err
	}
	return nil
}

func (h *Handler) UnlockUser(ctx context.Context, params apiv1.UnlockUserParams) error {
	if err := requireRootPeer(ctx); err != nil {
		return err
	}
	if h.Auth == nil {
		return fmt.Errorf("auth service not configured")
	}
	if err := h.Auth.UnlockUser(ctx, params.Username); err != nil {
		return err
	}
	detail := fmt.Sprintf("user %q login lockout cleared", params.Username)
	if err := h.auditRecovery(ctx, "user_unlock", detail); err != nil {
		return err
	}
	title := "Account lockout cleared"
	msg := fmt.Sprintf("The login lockout for user %q was cleared by root over the Unix socket.", params.Username)
	return h.announceRecovery(ctx, title, msg)
}
