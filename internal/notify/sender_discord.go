package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// DiscordSender posts to a Discord webhook (doc 03 §8.3). The webhook URL
// itself authenticates the request, so it is treated as the channel's
// credential (Q28) rather than a plain config field — cfg carries no
// Discord-specific fields at all.
type DiscordSender struct {
	// Client defaults to http.DefaultClient.
	Client HTTPDoer
}

var _ Sender = (*DiscordSender)(nil)

func (d *DiscordSender) client() HTTPDoer {
	if d.Client != nil {
		return d.Client
	}
	return http.DefaultClient
}

func (d *DiscordSender) Send(ctx context.Context, _ ChannelConfig, secret string, msg Message) error {
	if secret == "" {
		return fmt.Errorf("discord channel has no webhook url configured")
	}

	payload, err := json.Marshal(map[string]any{
		"content": fmt.Sprintf("**%s**\n%s", msg.Title, msg.Body),
	})
	if err != nil {
		return fmt.Errorf("encoding discord message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, secret, bytes.NewReader(payload))
	if err != nil {
		// err from NewRequestWithContext is a *url.Error carrying secret
		// (the webhook URL, the channel's credential, Q28) — never
		// surface it: this error reaches d.LastError, gets persisted by
		// UpdateDeliveryAttempt and can be returned by
		// sendTestNotification, none of which may carry a secret.
		return errors.New("building discord request: invalid webhook url")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client().Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("sending discord request: %w", urlErr.Err)
		}
		return fmt.Errorf("sending discord request: %w", err)
	}
	return checkHTTPStatus(resp)
}
