package api

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
)

func (h *Handler) ListAppUpdates(ctx context.Context) (*apiv1.ListAppUpdatesOK, error) {
	if h.AppUpdates == nil {
		return &apiv1.ListAppUpdatesOK{Available: false, Message: apiv1.NewOptString("Docker is not configured on this daemon")}, nil
	}
	statuses, err := h.AppUpdates.Statuses(ctx)
	if err != nil {
		if errors.Is(err, container.ErrUnavailable) {
			return &apiv1.ListAppUpdatesOK{Available: false, Message: unavailableAppsMessage(err)}, nil
		}
		return nil, fmt.Errorf("reading container update status: %w", err)
	}
	updates := make([]apiv1.AppUpdate, 0, len(statuses))
	for _, s := range statuses {
		u := apiv1.AppUpdate{Container: s.Container, Image: s.Image, Tag: s.Tag, Status: apiv1.AppUpdateStatus(s.Status)}
		if s.Kind != "" {
			u.Kind = apiv1.NewOptAppUpdateKind(apiv1.AppUpdateKind(s.Kind))
		}
		if s.AvailableTag != "" {
			u.AvailableTag = apiv1.NewOptString(s.AvailableTag)
		}
		if s.Message != "" {
			u.Message = apiv1.NewOptString(s.Message)
		}
		if !s.CheckedAt.IsZero() {
			u.CheckedAt = apiv1.NewOptDateTime(s.CheckedAt)
		}
		updates = append(updates, u)
	}
	return &apiv1.ListAppUpdatesOK{Available: true, Updates: updates}, nil
}
