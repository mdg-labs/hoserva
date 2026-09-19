package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"testing"
)

func TestGotifySenderSendsExpectedRequest(t *testing.T) {
	var gotPath, gotKey string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Gotify-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := &GotifySender{Client: srv.Client()}
	cfg := ChannelConfig{GotifyURL: srv.URL}
	err := sender.Send(context.Background(), cfg, "app-token", Message{Title: "Disk offline", Body: "sdb", Severity: SeverityCritical})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/message" {
		t.Fatalf("path = %q, want /message", gotPath)
	}
	if gotKey != "app-token" {
		t.Fatalf("X-Gotify-Key = %q, want app-token", gotKey)
	}
	if gotBody["title"] != "Disk offline" || gotBody["message"] != "sdb" {
		t.Fatalf("body = %+v", gotBody)
	}
}

func TestGotifySenderReportsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid token"))
	}))
	defer srv.Close()

	sender := &GotifySender{Client: srv.Client()}
	err := sender.Send(context.Background(), ChannelConfig{GotifyURL: srv.URL}, "bad", Message{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("Send: expected an error for a 401 response")
	}
	if !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("Send error = %q, want it to include the response body", err)
	}
}

func TestGotifySenderRequiresURL(t *testing.T) {
	sender := &GotifySender{}
	if err := sender.Send(context.Background(), ChannelConfig{}, "", Message{}); err == nil {
		t.Fatal("Send: expected an error for a missing gotifyUrl")
	}
}

func TestNtfySenderSendsExpectedRequest(t *testing.T) {
	var gotPath, gotTitle, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTitle = r.Header.Get("Title")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := &NtfySender{Client: srv.Client()}
	cfg := ChannelConfig{NtfyURL: srv.URL, NtfyTopic: "hoserva-alerts"}
	err := sender.Send(context.Background(), cfg, "ntfy-token", Message{Title: "Sync failed", Body: "details"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/hoserva-alerts" {
		t.Fatalf("path = %q, want /hoserva-alerts", gotPath)
	}
	if gotTitle != "Sync failed" {
		t.Fatalf("Title header = %q", gotTitle)
	}
	if gotAuth != "Bearer ntfy-token" {
		t.Fatalf("Authorization header = %q", gotAuth)
	}
}

func TestNtfySenderRequiresURLAndTopic(t *testing.T) {
	sender := &NtfySender{}
	if err := sender.Send(context.Background(), ChannelConfig{}, "", Message{}); err == nil {
		t.Fatal("Send: expected an error for a missing ntfyUrl")
	}
	if err := sender.Send(context.Background(), ChannelConfig{NtfyURL: "http://ntfy.example"}, "", Message{}); err == nil {
		t.Fatal("Send: expected an error for a missing ntfyTopic")
	}
}

func TestDiscordSenderUsesSecretAsURL(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// Discord's webhook URL is itself the credential (Q28) — the fake
	// server's own URL stands in for it here.
	sender := &DiscordSender{Client: srv.Client()}
	err := sender.Send(context.Background(), ChannelConfig{}, srv.URL, Message{Title: "Array degraded", Body: "disk 3 missing"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	content, _ := gotBody["content"].(string)
	if !strings.Contains(content, "Array degraded") || !strings.Contains(content, "disk 3 missing") {
		t.Fatalf("content = %q", content)
	}
}

func TestDiscordSenderRequiresSecret(t *testing.T) {
	sender := &DiscordSender{}
	if err := sender.Send(context.Background(), ChannelConfig{}, "", Message{}); err == nil {
		t.Fatal("Send: expected an error for a missing webhook url/secret")
	}
}

func TestWebhookSenderSendsHeadersAndAuth(t *testing.T) {
	var gotMethod, gotCustom, gotAuth string
	var gotBody webhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCustom = r.Header.Get("X-Custom")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := &WebhookSender{Client: srv.Client()}
	cfg := ChannelConfig{
		WebhookURL:            srv.URL,
		WebhookMethod:         "PUT",
		WebhookHeaders:        map[string]string{"X-Custom": "yes"},
		WebhookAuthHeaderName: "Authorization",
	}
	err := sender.Send(context.Background(), cfg, "secret-token", Message{EventType: EventDiskOffline, Severity: SeverityCritical, Title: "Disk offline", Body: "sdb"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method = %q, want PUT", gotMethod)
	}
	if gotCustom != "yes" {
		t.Fatalf("X-Custom header = %q, want yes", gotCustom)
	}
	if gotAuth != "secret-token" {
		t.Fatalf("Authorization header = %q, want secret-token", gotAuth)
	}
	if gotBody.Event != string(EventDiskOffline) || gotBody.Severity != string(SeverityCritical) {
		t.Fatalf("body = %+v", gotBody)
	}
}

func TestWebhookSenderDefaultsToPost(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := &WebhookSender{Client: srv.Client()}
	err := sender.Send(context.Background(), ChannelConfig{WebhookURL: srv.URL}, "", Message{Title: "t", Body: "b"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST (default)", gotMethod)
	}
}

func TestWebhookSenderRequiresURL(t *testing.T) {
	sender := &WebhookSender{}
	if err := sender.Send(context.Background(), ChannelConfig{}, "", Message{}); err == nil {
		t.Fatal("Send: expected an error for a missing webhookUrl")
	}
}

func TestEmailSenderBuildsMessageAndCallsSendMail(t *testing.T) {
	var gotAddr string
	var gotFrom string
	var gotTo []string
	var gotMsg []byte
	sender := &EmailSender{
		SendMail: func(_ context.Context, addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
			gotAddr, gotFrom, gotTo, gotMsg = addr, from, to, msg
			return nil
		},
	}
	cfg := ChannelConfig{
		EmailHost: "smtp.example.com", EmailPort: 587,
		EmailFrom: "hoserva@example.com", EmailTo: []string{"ops@example.com"},
		EmailUsername: "hoserva",
	}
	err := sender.Send(context.Background(), cfg, "app-password", Message{Title: "Sync failed", Body: "details here"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAddr != "smtp.example.com:587" {
		t.Fatalf("addr = %q", gotAddr)
	}
	if gotFrom != "hoserva@example.com" {
		t.Fatalf("from = %q", gotFrom)
	}
	if len(gotTo) != 1 || gotTo[0] != "ops@example.com" {
		t.Fatalf("to = %v", gotTo)
	}
	if !strings.Contains(string(gotMsg), "Subject: Sync failed") || !strings.Contains(string(gotMsg), "details here") {
		t.Fatalf("message body = %q", gotMsg)
	}
}

var errFakeSMTP = errors.New("fake smtp failure")

func TestEmailSenderPropagatesError(t *testing.T) {
	sender := &EmailSender{
		SendMail: func(_ context.Context, addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
			return errFakeSMTP
		},
	}
	cfg := ChannelConfig{EmailHost: "smtp.example.com", EmailPort: 25, EmailFrom: "a@example.com", EmailTo: []string{"b@example.com"}}
	if err := sender.Send(context.Background(), cfg, "", Message{}); err == nil {
		t.Fatal("Send: expected an error")
	} else if !errors.Is(err, errFakeSMTP) {
		t.Fatalf("Send error = %v, want it to wrap errFakeSMTP", err)
	}
}

func TestEmailSenderRequiresConfig(t *testing.T) {
	sender := &EmailSender{SendMail: func(context.Context, string, smtp.Auth, string, []string, []byte) error { return nil }}
	if err := sender.Send(context.Background(), ChannelConfig{}, "", Message{}); err == nil {
		t.Fatal("Send: expected an error for missing email config")
	}
}
