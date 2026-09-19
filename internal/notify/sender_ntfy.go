package notify

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// NtfySender posts to an ntfy topic (doc 03 §8.3): a plain-text POST body,
// with the title and priority carried in headers per ntfy's own publish
// API.
type NtfySender struct {
	// Client defaults to http.DefaultClient.
	Client HTTPDoer
}

var _ Sender = (*NtfySender)(nil)

func (n *NtfySender) client() HTTPDoer {
	if n.Client != nil {
		return n.Client
	}
	return http.DefaultClient
}

// ntfyPriority maps Severity onto ntfy's 1(min)-5(urgent) priority scale.
func ntfyPriority(s Severity) int {
	switch s {
	case SeverityCritical:
		return 5
	case SeverityError:
		return 4
	case SeverityWarning:
		return 3
	default:
		return 2
	}
}

func (n *NtfySender) Send(ctx context.Context, cfg ChannelConfig, secret string, msg Message) error {
	if cfg.NtfyURL == "" {
		return fmt.Errorf("ntfy channel has no url configured")
	}
	if cfg.NtfyTopic == "" {
		return fmt.Errorf("ntfy channel has no topic configured")
	}

	url := strings.TrimRight(cfg.NtfyURL, "/") + "/" + cfg.NtfyTopic
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(msg.Body))
	if err != nil {
		return fmt.Errorf("building ntfy request: %w", err)
	}
	req.Header.Set("Title", msg.Title)
	req.Header.Set("Priority", strconv.Itoa(ntfyPriority(msg.Severity)))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	resp, err := n.client().Do(req)
	if err != nil {
		return fmt.Errorf("sending ntfy request: %w", err)
	}
	return checkHTTPStatus(resp)
}
