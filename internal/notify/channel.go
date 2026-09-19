package notify

import (
	"fmt"
	"time"
)

// ChannelType is one of doc 03 §8.3's five alerting destinations.
type ChannelType string

const (
	ChannelEmail   ChannelType = "email"
	ChannelGotify  ChannelType = "gotify"
	ChannelNtfy    ChannelType = "ntfy"
	ChannelDiscord ChannelType = "discord"
	ChannelWebhook ChannelType = "webhook"
)

// ValidChannelType reports whether t is one of the five channel types.
func ValidChannelType(t ChannelType) bool {
	switch t {
	case ChannelEmail, ChannelGotify, ChannelNtfy, ChannelDiscord, ChannelWebhook:
		return true
	default:
		return false
	}
}

// ChannelConfig is every non-secret, type-specific setting a channel can
// carry. Only the fields its own Type uses are meaningful; the rest are
// zero. This is what internal/notify's Store JSON-encodes into
// notify_channels.config (Q60/D4 — config, not the credential, which is
// encrypted separately).
type ChannelConfig struct {
	EmailHost             string            `json:"emailHost,omitempty"`
	EmailPort             int               `json:"emailPort,omitempty"`
	EmailUsername         string            `json:"emailUsername,omitempty"`
	EmailFrom             string            `json:"emailFrom,omitempty"`
	EmailTo               []string          `json:"emailTo,omitempty"`
	EmailStartTLS         bool              `json:"emailStartTls,omitempty"`
	GotifyURL             string            `json:"gotifyUrl,omitempty"`
	NtfyURL               string            `json:"ntfyUrl,omitempty"`
	NtfyTopic             string            `json:"ntfyTopic,omitempty"`
	WebhookURL            string            `json:"webhookUrl,omitempty"`
	WebhookMethod         string            `json:"webhookMethod,omitempty"`
	WebhookHeaders        map[string]string `json:"webhookHeaders,omitempty"`
	WebhookAuthHeaderName string            `json:"webhookAuthHeaderName,omitempty"`
}

// SecretOp names what ChannelInput.Secret asks the store to do with a
// channel's credential — the tri-state UpdateNotificationChannelRequest's
// own `secret` field needs (omitted/null/string), carried as a Go type
// instead of a *string so "clear it" and "leave it alone" are never
// confused (Q28).
type SecretOp int

const (
	// SecretKeep leaves an existing credential untouched. The only valid
	// op for CreateChannel, where there is nothing yet to keep — Store
	// treats it identically to SecretClear there.
	SecretKeep SecretOp = iota
	// SecretClear removes a channel's credential.
	SecretClear
	// SecretSet replaces a channel's credential with Value.
	SecretSet
)

// SecretInput carries a channel-credential change. A nil *SecretInput on
// ChannelInput means SecretKeep.
type SecretInput struct {
	Op    SecretOp
	Value string
}

// ChannelInput is what CreateChannel/UpdateChannel accept — the same
// shape api/openapi.yaml's Create/UpdateNotificationChannelRequest
// carries, translated by internal/api into this package's own types so
// nothing here depends on generated API code (D18: the dependency runs
// one way, api -> notify, never back).
type ChannelInput struct {
	Name    string
	Type    ChannelType
	Enabled bool
	Config  ChannelConfig
	Secret  *SecretInput
}

// Validate checks the non-secret fields Type actually uses. It
// deliberately does not require a credential to be present — a channel
// can be created or left disabled without one, and sendTestNotification
// (doc 03 §8.3: "untested notification config is the same as no
// notification config") is what surfaces a missing or wrong one.
func (in ChannelInput) Validate() error {
	if in.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !ValidChannelType(in.Type) {
		return fmt.Errorf("invalid channel type %q", in.Type)
	}
	switch in.Type {
	case ChannelEmail:
		if in.Config.EmailHost == "" {
			return fmt.Errorf("emailHost is required for an email channel")
		}
		if in.Config.EmailPort < 1 || in.Config.EmailPort > 65535 {
			return fmt.Errorf("emailPort must be between 1 and 65535 for an email channel")
		}
		if in.Config.EmailFrom == "" {
			return fmt.Errorf("emailFrom is required for an email channel")
		}
		if len(in.Config.EmailTo) == 0 {
			return fmt.Errorf("emailTo must name at least one recipient for an email channel")
		}
	case ChannelGotify:
		if in.Config.GotifyURL == "" {
			return fmt.Errorf("gotifyUrl is required for a gotify channel")
		}
	case ChannelNtfy:
		if in.Config.NtfyURL == "" {
			return fmt.Errorf("ntfyUrl is required for an ntfy channel")
		}
		if in.Config.NtfyTopic == "" {
			return fmt.Errorf("ntfyTopic is required for an ntfy channel")
		}
	case ChannelWebhook:
		if in.Config.WebhookURL == "" {
			return fmt.Errorf("webhookUrl is required for a webhook channel")
		}
		if in.Config.WebhookMethod != "" && in.Config.WebhookMethod != "POST" && in.Config.WebhookMethod != "PUT" {
			return fmt.Errorf("webhookMethod must be POST or PUT")
		}
	case ChannelDiscord:
		// No non-secret config: the webhook URL itself is the credential.
	}
	return nil
}

// Channel is a notify_channels row, translated out of internal storage —
// it never carries a credential (Q28); HasSecret is the only signal that
// one is configured.
type Channel struct {
	ID        string
	Name      string
	Type      ChannelType
	Enabled   bool
	Config    ChannelConfig
	HasSecret bool
	CreatedAt time.Time
	UpdatedAt time.Time
}
