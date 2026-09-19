package main

import (
	"context"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// Recovery operations are root-only over the Unix socket (Q78). The mock
// has no peer-credential layer, so every call is refused with the same
// forbidden response the real daemon returns to a non-root peer.

func (h *handler) ResetUserPassword(ctx context.Context, req *apiv1.ResetUserPasswordRequest, params apiv1.ResetUserPasswordParams) error {
	return errRootOnlyRecovery()
}

func (h *handler) DisableUserTotp(ctx context.Context, params apiv1.DisableUserTotpParams) error {
	return errRootOnlyRecovery()
}

func (h *handler) UnlockUser(ctx context.Context, params apiv1.UnlockUserParams) error {
	return errRootOnlyRecovery()
}
