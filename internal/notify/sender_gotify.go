package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// GotifySender posts to a Gotify server's message API (doc 03 §8.3).
type GotifySender struct {
	// Client defaults to http.DefaultClient.
	Client HTTPDoer
}

var _ Sender = (*GotifySender)(nil)

func (g *GotifySender) client() HTTPDoer {
	if g.Client != nil {
		return g.Client
	}
	return http.DefaultClient
}

// gotifyPriority maps Severity onto Gotify's 0-10 integer priority scale
// — high enough for Severity's own top two levels to trigger a Gotify
// client's own high-priority notification behaviour.
func gotifyPriority(s Severity) int {
	switch s {
	case SeverityCritical:
		return 9
	case SeverityError:
		return 7
	case SeverityWarning:
		return 5
	default:
		return 2
	}
}

func (g *GotifySender) Send(ctx context.Context, cfg ChannelConfig, secret string, msg Message) error {
	if cfg.GotifyURL == "" {
		return fmt.Errorf("gotify channel has no url configured")
	}

	payload, err := json.Marshal(map[string]any{
		"title":    msg.Title,
		"message":  msg.Body,
		"priority": gotifyPriority(msg.Severity),
	})
	if err != nil {
		return fmt.Errorf("encoding gotify message: %w", err)
	}

	url := strings.TrimRight(cfg.GotifyURL, "/") + "/message"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("building gotify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Gotify-Key", secret)
	}

	resp, err := g.client().Do(req)
	if err != nil {
		return fmt.Errorf("sending gotify request: %w", err)
	}
	return checkHTTPStatus(resp)
}
