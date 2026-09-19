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

// recoveryAuditTestHook, when non-nil, is called by auditRecoveryWithStore
// instead of inserting the audit row. Tests use SetRecoveryAuditTestHook.
var recoveryAuditTestHook func() error

// SetRecoveryAuditTestHook configures a test-only hook that makes the next
// recovery audit insert fail. Pass nil to clear.
func SetRecoveryAuditTestHook(fn func() error) {
	recoveryAuditTestHook = fn
}

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

type recoveryOutcome struct {
	limiterSubject string
	limiterKind    auth.SubjectKind
}

func (h *Handler) performRecovery(
	ctx context.Context,
	mutate func(txStore *AuthStore) (*recoveryOutcome, error),
	auditAction, auditDetail, title, msg string,
) error {
	if h.Auth == nil || h.Auth.Store == nil {
		return fmt.Errorf("auth service not configured")
	}
	var outcome *recoveryOutcome
	err := h.Auth.Store.RunRecoveryTx(ctx, func(txStore *AuthStore) error {
		o, err := mutate(txStore)
		if err != nil {
			return err
		}
		outcome = o
		if err := h.auditRecoveryWithStore(ctx, txStore, auditAction, auditDetail); err != nil {
			return err
		}
		return h.announceRecoveryWithStore(ctx, txStore, title, msg)
	})
	if err != nil {
		return err
	}
	if outcome != nil && outcome.limiterSubject != "" {
		h.Auth.Limiter.Clear(outcome.limiterSubject, outcome.limiterKind)
	}
	return nil
}

func (h *Handler) auditRecoveryWithStore(ctx context.Context, store *AuthStore, action, detail string) error {
	if recoveryAuditTestHook != nil {
		return recoveryAuditTestHook()
	}
	now := time.Now()
	if h.Auth != nil && h.Auth.Now != nil {
		now = h.Auth.Now()
	}
	return store.InsertAuditLog(ctx, recoveryActor(), action, detail, now)
}

func (h *Handler) announceRecoveryWithStore(ctx context.Context, store *AuthStore, title, message string) error {
	if h.Notify == nil {
		return nil
	}
	channels, err := h.Notify.ListChannels(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	if h.Auth != nil && h.Auth.Now != nil {
		now = h.Auth.Now()
	}
	nowStr := now.UTC().Format(timeFormat)
	var errs []error
	for _, ch := range channels {
		if !ch.Enabled {
			continue
		}
		if err := store.CreateRecoveryDelivery(ctx, uuid.NewString(), ch.ID, title, message, nowStr); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("queuing recovery notifications: %v", errs)
	}
	return nil
}
