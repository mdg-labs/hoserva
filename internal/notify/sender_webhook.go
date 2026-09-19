package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// WebhookSender posts to a user-configured generic webhook (doc 03 §8.3):
// a structured JSON body a receiving script can parse without guessing at
// a vendor-specific shape, never a shell command built from any of this —
// nothing here ever reaches an exec call (CLAUDE.md).
type WebhookSender struct {
	// Client defaults to http.DefaultClient.
	Client HTTPDoer
}

var _ Sender = (*WebhookSender)(nil)

func (w *WebhookSender) client() HTTPDoer {
	if w.Client != nil {
		return w.Client
	}
	return http.DefaultClient
}

type webhookPayload struct {
	Event    string `json:"event"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Message  string `json:"message"`
}

func (w *WebhookSender) Send(ctx context.Context, cfg ChannelConfig, secret string, msg Message) error {
	if cfg.WebhookURL == "" {
		return fmt.Errorf("webhook channel has no url configured")
	}
	method := cfg.WebhookMethod
	if method == "" {
		method = http.MethodPost
	}

	body, err := json.Marshal(webhookPayload{
		Event:    string(msg.EventType),
		Severity: string(msg.Severity),
		Title:    msg.Title,
		Message:  msg.Body,
	})
	if err != nil {
		return fmt.Errorf("encoding webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range cfg.WebhookHeaders {
		req.Header.Set(name, value)
	}
	if cfg.WebhookAuthHeaderName != "" && secret != "" {
		req.Header.Set(cfg.WebhookAuthHeaderName, secret)
	}

	resp, err := w.client().Do(req)
	if err != nil {
		return fmt.Errorf("sending webhook request: %w", err)
	}
	return checkHTTPStatus(resp)
}
