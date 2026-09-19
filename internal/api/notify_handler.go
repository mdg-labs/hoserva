package api

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// mapNotifyError classifies every error internal/notify.Service can
// return into the spec's shared Error schema (doc 01 §5), the same role
// mapStoreError/mapSchedulerError play for the job system.
func mapNotifyError(err error) error {
	switch {
	case errors.Is(err, notify.ErrChannelNotFound):
		return &apiError{code: "notification_channel_not_found", statusCode: 404, message: "no notification channel with that id"}
	case errors.Is(err, notify.ErrUnknownChannel):
		return &apiError{code: "notification_unknown_channel", statusCode: 400, message: err.Error()}
	case errors.Is(err, notify.ErrInvalidInput):
		return &apiError{code: "notification_invalid_input", statusCode: 400, message: err.Error()}
	default:
		return err
	}
}

func (h *Handler) ListNotificationChannels(ctx context.Context) (*apiv1.ListNotificationChannelsOK, error) {
	channels, err := h.Notify.ListChannels(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing notification channels: %w", err)
	}
	return &apiv1.ListNotificationChannelsOK{Channels: channelsToAPI(channels)}, nil
}

func (h *Handler) GetNotificationChannel(ctx context.Context, params apiv1.GetNotificationChannelParams) (*apiv1.NotificationChannel, error) {
	ch, err := h.Notify.GetChannel(ctx, params.ChannelId.String())
	if err != nil {
		return nil, mapNotifyError(err)
	}
	out := channelToAPI(ch)
	return &out, nil
}

func (h *Handler) CreateNotificationChannel(ctx context.Context, req *apiv1.CreateNotificationChannelRequest) (*apiv1.NotificationChannel, error) {
	ch, err := h.Notify.CreateChannel(ctx, channelInputFromCreate(req))
	if err != nil {
		return nil, mapNotifyError(err)
	}
	out := channelToAPI(ch)
	return &out, nil
}

func (h *Handler) UpdateNotificationChannel(ctx context.Context, req *apiv1.UpdateNotificationChannelRequest, params apiv1.UpdateNotificationChannelParams) (*apiv1.NotificationChannel, error) {
	ch, err := h.Notify.UpdateChannel(ctx, params.ChannelId.String(), channelInputFromUpdate(req))
	if err != nil {
		return nil, mapNotifyError(err)
	}
	out := channelToAPI(ch)
	return &out, nil
}

func (h *Handler) DeleteNotificationChannel(ctx context.Context, params apiv1.DeleteNotificationChannelParams) error {
	if err := h.Notify.DeleteChannel(ctx, params.ChannelId.String()); err != nil {
		return mapNotifyError(err)
	}
	return nil
}

func (h *Handler) SendTestNotification(ctx context.Context, params apiv1.SendTestNotificationParams) (*apiv1.NotificationTestResult, error) {
	result, err := h.Notify.TestChannel(ctx, params.ChannelId.String())
	if err != nil {
		return nil, mapNotifyError(err)
	}
	out := &apiv1.NotificationTestResult{Success: result.Success}
	if !result.Success {
		out.Error = apiv1.NewOptNilString(result.Error)
	}
	return out, nil
}

func (h *Handler) GetNotificationRouting(ctx context.Context) (*apiv1.GetNotificationRoutingOK, error) {
	routing, err := h.Notify.GetRouting(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting notification routing: %w", err)
	}
	return &apiv1.GetNotificationRoutingOK{Routing: routingEntriesToAPI(routing)}, nil
}

func (h *Handler) UpdateNotificationRoute(ctx context.Context, req *apiv1.UpdateNotificationRouteRequest, params apiv1.UpdateNotificationRouteParams) (*apiv1.NotificationRoutingEntry, error) {
	entry, err := h.Notify.SetRoute(ctx, notify.EventType(params.EventType), notify.Severity(req.Severity), channelIDsFromAPI(req.ChannelIds))
	if err != nil {
		return nil, mapNotifyError(err)
	}
	out := routingEntryToAPI(*entry)
	return &out, nil
}

func (h *Handler) GetQuietHours(ctx context.Context) (*apiv1.NotificationQuietHours, error) {
	qh, err := h.Notify.GetQuietHours(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting quiet hours: %w", err)
	}
	out := quietHoursToAPI(qh)
	return &out, nil
}

func (h *Handler) UpdateQuietHours(ctx context.Context, req *apiv1.UpdateQuietHoursRequest) (*apiv1.NotificationQuietHours, error) {
	qh, err := h.Notify.SetQuietHours(ctx, req.Enabled, req.Start, req.End)
	if err != nil {
		return nil, mapNotifyError(err)
	}
	out := quietHoursToAPI(qh)
	return &out, nil
}

func (h *Handler) ListNotifications(ctx context.Context) (*apiv1.ListNotificationsOK, error) {
	groups, unread, err := h.Notify.ListInbox(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing notifications: %w", err)
	}
	return &apiv1.ListNotificationsOK{
		Groups:      alertGroupsToAPI(groups),
		UnreadCount: unread,
	}, nil
}

func (h *Handler) MarkNotificationsRead(ctx context.Context, req *apiv1.MarkNotificationsReadRequest) (*apiv1.MarkNotificationsReadOK, error) {
	all := false
	if v, ok := req.All.Get(); ok {
		all = v
	}
	unread, err := h.Notify.MarkRead(ctx, req.Ids, all)
	if err != nil {
		return nil, fmt.Errorf("marking notifications read: %w", err)
	}
	return &apiv1.MarkNotificationsReadOK{UnreadCount: unread}, nil
}
