package api

import (
	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// channelConfigFields is the getter shape api/openapi.yaml's
// CreateNotificationChannelRequest, UpdateNotificationChannelRequest and
// NotificationChannel all share for every type-specific config field —
// ogen generates the same method set (and, since NotificationWebhookMethod
// and NotificationWebhookHeaders are $ref'd shared schemas rather than
// three separately generated inline ones, the same types) on all three,
// so channelConfigFromFields below reads any of them through this one
// interface instead of three near-identical conversion functions.
type channelConfigFields interface {
	GetEmailHost() apiv1.OptString
	GetEmailPort() apiv1.OptInt32
	GetEmailUsername() apiv1.OptString
	GetEmailFrom() apiv1.OptString
	GetEmailTo() []string
	GetEmailStartTls() apiv1.OptBool
	GetGotifyUrl() apiv1.OptString
	GetNtfyUrl() apiv1.OptString
	GetNtfyTopic() apiv1.OptString
	GetWebhookUrl() apiv1.OptString
	GetWebhookMethod() apiv1.OptNotificationWebhookMethod
	GetWebhookHeaders() apiv1.OptNotificationWebhookHeaders
	GetWebhookAuthHeaderName() apiv1.OptString
}

func channelConfigFromFields(f channelConfigFields) notify.ChannelConfig {
	cfg := notify.ChannelConfig{
		EmailHost:             f.GetEmailHost().Value,
		EmailPort:             int(f.GetEmailPort().Value),
		EmailUsername:         f.GetEmailUsername().Value,
		EmailFrom:             f.GetEmailFrom().Value,
		EmailTo:               f.GetEmailTo(),
		EmailStartTLS:         f.GetEmailStartTls().Value,
		GotifyURL:             f.GetGotifyUrl().Value,
		NtfyURL:               f.GetNtfyUrl().Value,
		NtfyTopic:             f.GetNtfyTopic().Value,
		WebhookURL:            f.GetWebhookUrl().Value,
		WebhookMethod:         string(f.GetWebhookMethod().Value),
		WebhookAuthHeaderName: f.GetWebhookAuthHeaderName().Value,
	}
	if headers, ok := f.GetWebhookHeaders().Get(); ok {
		cfg.WebhookHeaders = headers
	}
	return cfg
}

// channelToAPI translates a *notify.Channel into the generated
// NotificationChannel — never carrying a credential (Q28); HasSecret is
// the only trace of one.
func channelToAPI(ch *notify.Channel) apiv1.NotificationChannel {
	out := apiv1.NotificationChannel{
		ID:        uuid.MustParse(ch.ID),
		Name:      ch.Name,
		Type:      apiv1.NotificationChannelType(ch.Type),
		Enabled:   ch.Enabled,
		HasSecret: ch.HasSecret,
		EmailTo:   ch.Config.EmailTo,
		CreatedAt: ch.CreatedAt,
		UpdatedAt: ch.UpdatedAt,
	}
	if ch.Config.EmailHost != "" {
		out.EmailHost = apiv1.NewOptString(ch.Config.EmailHost)
	}
	if ch.Config.EmailPort != 0 {
		out.EmailPort = apiv1.NewOptInt32(int32(ch.Config.EmailPort))
	}
	if ch.Config.EmailUsername != "" {
		out.EmailUsername = apiv1.NewOptString(ch.Config.EmailUsername)
	}
	if ch.Config.EmailFrom != "" {
		out.EmailFrom = apiv1.NewOptString(ch.Config.EmailFrom)
	}
	if ch.Config.EmailStartTLS {
		out.EmailStartTls = apiv1.NewOptBool(true)
	}
	if ch.Config.GotifyURL != "" {
		out.GotifyUrl = apiv1.NewOptString(ch.Config.GotifyURL)
	}
	if ch.Config.NtfyURL != "" {
		out.NtfyUrl = apiv1.NewOptString(ch.Config.NtfyURL)
	}
	if ch.Config.NtfyTopic != "" {
		out.NtfyTopic = apiv1.NewOptString(ch.Config.NtfyTopic)
	}
	if ch.Config.WebhookURL != "" {
		out.WebhookUrl = apiv1.NewOptString(ch.Config.WebhookURL)
	}
	if ch.Config.WebhookMethod != "" {
		out.WebhookMethod = apiv1.NewOptNotificationWebhookMethod(apiv1.NotificationWebhookMethod(ch.Config.WebhookMethod))
	}
	if len(ch.Config.WebhookHeaders) > 0 {
		out.WebhookHeaders = apiv1.NewOptNotificationWebhookHeaders(ch.Config.WebhookHeaders)
	}
	if ch.Config.WebhookAuthHeaderName != "" {
		out.WebhookAuthHeaderName = apiv1.NewOptString(ch.Config.WebhookAuthHeaderName)
	}
	return out
}

func channelsToAPI(channels []*notify.Channel) []apiv1.NotificationChannel {
	out := make([]apiv1.NotificationChannel, 0, len(channels))
	for _, ch := range channels {
		out = append(out, channelToAPI(ch))
	}
	return out
}

// secretInputFromCreate reads CreateNotificationChannelRequest's own
// two-state `secret` (absent/string — no null case in a create body):
// absent means no credential at all, matching notify.SecretKeep's own
// behaviour on a fresh channel.
func secretInputFromCreate(req *apiv1.CreateNotificationChannelRequest) *notify.SecretInput {
	if value, ok := req.Secret.Get(); ok {
		return &notify.SecretInput{Op: notify.SecretSet, Value: value}
	}
	return nil
}

// secretInputFromUpdate reads UpdateNotificationChannelRequest's tri-state
// `secret` (omitted/null/string — Q28): omitted is nil (SecretKeep), null
// is SecretClear, a string is SecretSet.
func secretInputFromUpdate(req *apiv1.UpdateNotificationChannelRequest) *notify.SecretInput {
	if !req.Secret.Set {
		return nil
	}
	if req.Secret.Null {
		return &notify.SecretInput{Op: notify.SecretClear}
	}
	return &notify.SecretInput{Op: notify.SecretSet, Value: req.Secret.Value}
}

func channelInputFromCreate(req *apiv1.CreateNotificationChannelRequest) notify.ChannelInput {
	return notify.ChannelInput{
		Name:    req.Name,
		Type:    notify.ChannelType(req.Type),
		Enabled: req.Enabled,
		Config:  channelConfigFromFields(req),
		Secret:  secretInputFromCreate(req),
	}
}

func channelInputFromUpdate(req *apiv1.UpdateNotificationChannelRequest) notify.ChannelInput {
	return notify.ChannelInput{
		Name:    req.Name,
		Type:    notify.ChannelType(req.Type),
		Enabled: req.Enabled,
		Config:  channelConfigFromFields(req),
		Secret:  secretInputFromUpdate(req),
	}
}

func routingEntryToAPI(e notify.RoutingEntry) apiv1.NotificationRoutingEntry {
	ids := make([]uuid.UUID, 0, len(e.ChannelIDs))
	for _, id := range e.ChannelIDs {
		ids = append(ids, uuid.MustParse(id))
	}
	return apiv1.NotificationRoutingEntry{
		EventType:  apiv1.NotificationEventType(e.EventType),
		Severity:   apiv1.NotificationLevel(e.Severity),
		ChannelIds: ids,
	}
}

func routingEntriesToAPI(entries []notify.RoutingEntry) []apiv1.NotificationRoutingEntry {
	out := make([]apiv1.NotificationRoutingEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, routingEntryToAPI(e))
	}
	return out
}

func channelIDsFromAPI(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// quietHoursToAPI attaches the undisable critical-alert override
// (doc 03 §8.3, CLAUDE.md) — always true, and not a stored value.
func quietHoursToAPI(qh notify.QuietHours) apiv1.NotificationQuietHours {
	return apiv1.NotificationQuietHours{
		Enabled:                qh.Enabled,
		Start:                  qh.StartTime,
		End:                    qh.EndTime,
		CriticalAlwaysDelivers: true,
	}
}
