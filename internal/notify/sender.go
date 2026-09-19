package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Message is what Sender.Send actually delivers — everything a channel
// needs to render one notification, independent of that channel's own
// config/credential (both passed alongside it).
type Message struct {
	EventType EventType
	Severity  Severity
	Title     string
	Body      string
}

// Sender delivers one Message through one channel's config and decrypted
// credential (secret is "" for a channel with none configured). Each
// channel type's real implementation is a small, separately testable
// system-touching component (CLAUDE.md) behind this same interface, so
// Service and Worker never depend on net/smtp or net/http directly.
type Sender interface {
	Send(ctx context.Context, cfg ChannelConfig, secret string, msg Message) error
}

// HTTPDoer is satisfied by *http.Client — the seam every HTTP-based
// sender (Gotify, ntfy, Discord, generic webhook) takes instead of
// reaching for http.DefaultClient directly, so tests substitute a fake
// that never opens a real connection.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// checkHTTPStatus reads and closes resp.Body, returning an error naming
// the status code and, if the body is small and looks readable, its
// content — the detail a "Send test notification" response (doc 03 §8.3)
// or a delivery's last_error should carry, not just "non-2xx".
func checkHTTPStatus(resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if len(body) == 0 {
		return fmt.Errorf("received status %d", resp.StatusCode)
	}
	return fmt.Errorf("received status %d: %s", resp.StatusCode, body)
}

// Senders maps each ChannelType to the Sender that delivers through it —
// what Service.senderFor and Worker consult, so both share one wiring
// rather than each hardcoding a switch over ChannelType.
type Senders map[ChannelType]Sender

// DefaultSenders returns the real, network-touching Sender for every
// channel type, sharing one *http.Client (with a bounded per-request
// timeout, since a channel's own server hanging must never stall the
// delivery worker indefinitely).
func DefaultSenders(client HTTPDoer) Senders {
	return Senders{
		ChannelEmail:   &EmailSender{},
		ChannelGotify:  &GotifySender{Client: client},
		ChannelNtfy:    &NtfySender{Client: client},
		ChannelDiscord: &DiscordSender{Client: client},
		ChannelWebhook: &WebhookSender{Client: client},
	}
}

// FakeSender is a scriptable Sender for tests (CLAUDE.md): Err is
// returned verbatim from Send, and every call is recorded in Sent so a
// test can assert on what would have been delivered.
type FakeSender struct {
	Err  error
	Sent []Message
}

var _ Sender = (*FakeSender)(nil)

func (f *FakeSender) Send(_ context.Context, _ ChannelConfig, _ string, msg Message) error {
	f.Sent = append(f.Sent, msg)
	return f.Err
}
