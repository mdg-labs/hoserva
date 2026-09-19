package api

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mdg-labs/hoserva/internal/auth"
)

var (
	// ErrRootOnlyRecovery is returned when a Q78 recovery command is not
	// called by uid 0 over the Unix socket.
	ErrRootOnlyRecovery = &apiError{code: "forbidden", statusCode: 403, message: "this operation requires root over the Unix socket"}
	errUserNotFound     = &apiError{code: "user_not_found", statusCode: 404, message: "no such user"}
	errConfirmRequired  = &apiError{code: "confirmation_required", statusCode: 409, message: "this operation requires an explicit confirmation"}
)

// requireRootPeer refuses unless ctx carries a Unix socket peer credential
// with uid 0 (Q78) — hoserva-group and daemon-uid callers are not enough.
func requireRootPeer(ctx context.Context) error {
	cred, ok := auth.PeerCredentialFromContext(ctx)
	if !ok || cred.UID != 0 {
		return ErrRootOnlyRecovery
	}
	return nil
}

func recoveryActor() string {
	return "root"
}

func (h *Handler) auditRecovery(ctx context.Context, action, detail string) error {
	if h.Auth == nil || h.Auth.Store == nil {
		return fmt.Errorf("auth store not configured")
	}
	now := time.Now()
	if h.Auth.Now != nil {
		now = h.Auth.Now()
	}
	return h.Auth.Store.InsertAuditLog(ctx, recoveryActor(), action, detail, now)
}

func (h *Handler) announceRecovery(ctx context.Context, title, message string) error {
	if h.Notify == nil || h.Auth == nil || h.Auth.Store == nil {
		return nil
	}
	channels, err := h.Notify.ListChannels(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	if h.Auth.Now != nil {
		now = h.Auth.Now()
	}
	nowStr := now.UTC().Format(timeFormat)
	var errs []error
	for _, ch := range channels {
		if !ch.Enabled {
			continue
		}
		if err := h.Auth.Store.CreateRecoveryDelivery(ctx, uuid.NewString(), ch.ID, title, message, nowStr); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("queuing recovery notifications: %v", errs)
	}
	return nil
}
