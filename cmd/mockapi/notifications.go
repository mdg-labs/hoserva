package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// defaultNotificationRouting seeds one empty NotificationRoutingEntry per
// internal/notify.EventCatalog member, in that slice's own order, with
// its compiled-in default severity — mirroring what a real hoservad's
// GetNotificationRouting returns before any route has ever been set
// (#35), rather than inventing a second, possibly drifting event list
// and default-severity table here.
func defaultNotificationRouting() map[apiv1.NotificationEventType]apiv1.NotificationRoutingEntry {
	out := make(map[apiv1.NotificationEventType]apiv1.NotificationRoutingEntry, len(notify.EventCatalog))
	for _, event := range notify.EventCatalog {
		severity, _ := notify.DefaultSeverity(event)
		et := apiv1.NotificationEventType(event)
		out[et] = apiv1.NotificationRoutingEntry{
			EventType:  et,
			Severity:   apiv1.NotificationLevel(severity),
			ChannelIds: nil,
		}
	}
	return out
}

func errNotificationChannelNotFound(id uuid.UUID) error {
	return &mockError{code: "notification_channel_not_found", statusCode: 404, message: fmt.Sprintf("no notification channel with id %s", id)}
}

func (h *handler) ListNotificationChannels(ctx context.Context) (*apiv1.ListNotificationChannelsOK, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	out := make([]apiv1.NotificationChannel, 0, len(h.channels))
	for _, ch := range h.channels {
		out = append(out, ch)
	}
	return &apiv1.ListNotificationChannelsOK{Channels: out}, nil
}

func (h *handler) GetNotificationChannel(ctx context.Context, params apiv1.GetNotificationChannelParams) (*apiv1.NotificationChannel, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	ch, ok := h.channels[params.ChannelId]
	if !ok {
		return nil, errNotificationChannelNotFound(params.ChannelId)
	}
	return &ch, nil
}

func (h *handler) CreateNotificationChannel(ctx context.Context, req *apiv1.CreateNotificationChannelRequest) (*apiv1.NotificationChannel, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	now := time.Now().UTC()
	ch := apiv1.NotificationChannel{
		ID:                    uuid.New(),
		Name:                  req.Name,
		Type:                  req.Type,
		Enabled:               req.Enabled,
		HasSecret:             req.Secret.IsSet(),
		EmailHost:             req.EmailHost,
		EmailPort:             req.EmailPort,
		EmailUsername:         req.EmailUsername,
		EmailFrom:             req.EmailFrom,
		EmailTo:               req.EmailTo,
		EmailStartTls:         req.EmailStartTls,
		GotifyUrl:             req.GotifyUrl,
		NtfyUrl:               req.NtfyUrl,
		NtfyTopic:             req.NtfyTopic,
		WebhookUrl:            req.WebhookUrl,
		WebhookMethod:         req.WebhookMethod,
		WebhookHeaders:        req.WebhookHeaders,
		WebhookAuthHeaderName: req.WebhookAuthHeaderName,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	h.channels[ch.ID] = ch
	return &ch, nil
}

func (h *handler) UpdateNotificationChannel(ctx context.Context, req *apiv1.UpdateNotificationChannelRequest, params apiv1.UpdateNotificationChannelParams) (*apiv1.NotificationChannel, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	existing, ok := h.channels[params.ChannelId]
	if !ok {
		return nil, errNotificationChannelNotFound(params.ChannelId)
	}

	hasSecret := existing.HasSecret
	if req.Secret.Set {
		hasSecret = !req.Secret.Null
	}

	ch := apiv1.NotificationChannel{
		ID:                    existing.ID,
		Name:                  req.Name,
		Type:                  req.Type,
		Enabled:               req.Enabled,
		HasSecret:             hasSecret,
		EmailHost:             req.EmailHost,
		EmailPort:             req.EmailPort,
		EmailUsername:         req.EmailUsername,
		EmailFrom:             req.EmailFrom,
		EmailTo:               req.EmailTo,
		EmailStartTls:         req.EmailStartTls,
		GotifyUrl:             req.GotifyUrl,
		NtfyUrl:               req.NtfyUrl,
		NtfyTopic:             req.NtfyTopic,
		WebhookUrl:            req.WebhookUrl,
		WebhookMethod:         req.WebhookMethod,
		WebhookHeaders:        req.WebhookHeaders,
		WebhookAuthHeaderName: req.WebhookAuthHeaderName,
		CreatedAt:             existing.CreatedAt,
		UpdatedAt:             time.Now().UTC(),
	}
	h.channels[ch.ID] = ch
	return &ch, nil
}

func (h *handler) DeleteNotificationChannel(ctx context.Context, params apiv1.DeleteNotificationChannelParams) error {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	if _, ok := h.channels[params.ChannelId]; !ok {
		return errNotificationChannelNotFound(params.ChannelId)
	}
	delete(h.channels, params.ChannelId)

	for event, entry := range h.routing {
		filtered := entry.ChannelIds[:0]
		for _, id := range entry.ChannelIds {
			if id != params.ChannelId {
				filtered = append(filtered, id)
			}
		}
		entry.ChannelIds = filtered
		h.routing[event] = entry
	}
	return nil
}

// SendTestNotification always reports success for a channel that exists —
// there is no real network send to fail here, and no fixture scenario
// encodes a delivery failure to simulate one with.
func (h *handler) SendTestNotification(ctx context.Context, params apiv1.SendTestNotificationParams) (*apiv1.NotificationTestResult, error) {
	h.notifyMu.Lock()
	_, ok := h.channels[params.ChannelId]
	h.notifyMu.Unlock()
	if !ok {
		return nil, errNotificationChannelNotFound(params.ChannelId)
	}
	return &apiv1.NotificationTestResult{Success: true}, nil
}

func (h *handler) GetNotificationRouting(ctx context.Context) (*apiv1.GetNotificationRoutingOK, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	out := make([]apiv1.NotificationRoutingEntry, 0, len(notify.EventCatalog))
	for _, event := range notify.EventCatalog {
		out = append(out, h.routing[apiv1.NotificationEventType(event)])
	}
	return &apiv1.GetNotificationRoutingOK{Routing: out}, nil
}

func (h *handler) UpdateNotificationRoute(ctx context.Context, req *apiv1.UpdateNotificationRouteRequest, params apiv1.UpdateNotificationRouteParams) (*apiv1.NotificationRoutingEntry, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	entry := apiv1.NotificationRoutingEntry{
		EventType:  params.EventType,
		Severity:   req.Severity,
		ChannelIds: req.ChannelIds,
	}
	h.routing[params.EventType] = entry
	return &entry, nil
}

func (h *handler) GetQuietHours(ctx context.Context) (*apiv1.NotificationQuietHours, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	qh := h.quietHours
	return &qh, nil
}

func (h *handler) UpdateQuietHours(ctx context.Context, req *apiv1.UpdateQuietHoursRequest) (*apiv1.NotificationQuietHours, error) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	h.quietHours = apiv1.NotificationQuietHours{
		Enabled:                req.Enabled,
		Start:                  req.Start,
		End:                    req.End,
		CriticalAlwaysDelivers: true,
	}
	qh := h.quietHours
	return &qh, nil
}
