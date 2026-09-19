package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"
)

func newTestService(t *testing.T, senders Senders) *Service {
	t.Helper()
	svc := NewService(newTestStore(t), FakeSecretCipher{}, senders)
	svc.Log = func(string, ...any) {}
	return svc
}

func TestCreateChannelValidation(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)

	if _, err := svc.CreateChannel(ctx, ChannelInput{Name: "x", Type: ChannelEmail}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("CreateChannel: err = %v, want ErrInvalidInput", err)
	}

	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "Ops webhook", Type: ChannelWebhook, Enabled: true,
		Config: ChannelConfig{WebhookURL: "https://example.com/hook"},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if ch.HasSecret {
		t.Fatal("CreateChannel: HasSecret = true, want false (no secret supplied)")
	}
}

func TestCreateChannelEncryptsSecret(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)

	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "Discord", Type: ChannelDiscord, Enabled: true,
		Secret: &SecretInput{Op: SecretSet, Value: "https://discord.example/webhooks/1/abc"},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if !ch.HasSecret {
		t.Fatal("CreateChannel: HasSecret = false, want true")
	}

	raw, err := svc.Store.getChannelSecretCiphertext(ctx, ch.ID)
	if err != nil {
		t.Fatalf("getChannelSecretCiphertext: %v", err)
	}
	if string(raw) == "https://discord.example/webhooks/1/abc" {
		t.Fatal("channel secret is stored in plaintext, want ciphertext")
	}

	secret, err := svc.decryptChannelSecret(ctx, ch.ID)
	if err != nil {
		t.Fatalf("decryptChannelSecret: %v", err)
	}
	if secret != "https://discord.example/webhooks/1/abc" {
		t.Fatalf("decryptChannelSecret = %q, want original value", secret)
	}
}

func TestUpdateChannelSecretTriState(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)

	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "Discord", Type: ChannelDiscord, Enabled: true,
		Secret: &SecretInput{Op: SecretSet, Value: "https://discord.example/webhooks/1/abc"},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// Omitted (nil) secret input keeps the existing credential.
	updated, err := svc.UpdateChannel(ctx, ch.ID, ChannelInput{Name: "Discord 2", Type: ChannelDiscord, Enabled: true})
	if err != nil {
		t.Fatalf("UpdateChannel (keep): %v", err)
	}
	if !updated.HasSecret {
		t.Fatal("UpdateChannel (keep): HasSecret = false, want true")
	}
	secret, err := svc.decryptChannelSecret(ctx, ch.ID)
	if err != nil {
		t.Fatalf("decryptChannelSecret: %v", err)
	}
	if secret != "https://discord.example/webhooks/1/abc" {
		t.Fatalf("decryptChannelSecret after keep = %q, want unchanged", secret)
	}

	// Explicit clear removes it.
	updated, err = svc.UpdateChannel(ctx, ch.ID, ChannelInput{
		Name: "Discord 2", Type: ChannelDiscord, Enabled: true,
		Secret: &SecretInput{Op: SecretClear},
	})
	if err != nil {
		t.Fatalf("UpdateChannel (clear): %v", err)
	}
	if updated.HasSecret {
		t.Fatal("UpdateChannel (clear): HasSecret = true, want false")
	}

	// Explicit set replaces it.
	updated, err = svc.UpdateChannel(ctx, ch.ID, ChannelInput{
		Name: "Discord 2", Type: ChannelDiscord, Enabled: true,
		Secret: &SecretInput{Op: SecretSet, Value: "https://discord.example/webhooks/2/xyz"},
	})
	if err != nil {
		t.Fatalf("UpdateChannel (set): %v", err)
	}
	if !updated.HasSecret {
		t.Fatal("UpdateChannel (set): HasSecret = false, want true")
	}
	secret, err = svc.decryptChannelSecret(ctx, ch.ID)
	if err != nil {
		t.Fatalf("decryptChannelSecret: %v", err)
	}
	if secret != "https://discord.example/webhooks/2/xyz" {
		t.Fatalf("decryptChannelSecret after set = %q, want new value", secret)
	}
}

func TestSetRouteRejectsUnknownChannel(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)

	_, err := svc.SetRoute(ctx, EventDiskOffline, SeverityCritical, []string{"does-not-exist"})
	if !errors.Is(err, ErrUnknownChannel) {
		t.Fatalf("SetRoute: err = %v, want ErrUnknownChannel", err)
	}
}

func TestSetRouteRejectsInvalidEventOrSeverity(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)

	if _, err := svc.SetRoute(ctx, EventType("not_a_real_event"), SeverityCritical, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("SetRoute: err = %v, want ErrInvalidInput", err)
	}
	if _, err := svc.SetRoute(ctx, EventDiskOffline, Severity("urgent"), nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("SetRoute: err = %v, want ErrInvalidInput", err)
	}
}

func TestPublishQueuesOneDeliveryPerRoutedChannel(t *testing.T) {
	ctx := context.Background()
	sender := &FakeSender{}
	svc := newTestService(t, Senders{ChannelWebhook: sender})

	chA, err := svc.CreateChannel(ctx, ChannelInput{Name: "A", Type: ChannelWebhook, Enabled: true, Config: ChannelConfig{WebhookURL: "https://a.example/hook"}})
	if err != nil {
		t.Fatalf("CreateChannel A: %v", err)
	}
	chB, err := svc.CreateChannel(ctx, ChannelInput{Name: "B", Type: ChannelWebhook, Enabled: true, Config: ChannelConfig{WebhookURL: "https://b.example/hook"}})
	if err != nil {
		t.Fatalf("CreateChannel B: %v", err)
	}
	if _, err := svc.SetRoute(ctx, EventDiskOffline, SeverityCritical, []string{chA.ID, chB.ID}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}

	if err := svc.Publish(ctx, EventDiskOffline, "Disk offline", "sdb is offline"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	due, err := svc.Store.ListDueDeliveries(ctx, svc.now().Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListDueDeliveries: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("ListDueDeliveries: got %d, want 2 (one per routed channel)", len(due))
	}
}

func TestPublishWithNoRoutesQueuesNothing(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)

	// sync_succeeded is opt-in, off by default (doc 03 §8.3) — with no
	// routes configured for it, Publish must queue nothing at all, the
	// same as any other event type nobody has routed yet.
	if err := svc.Publish(ctx, EventSyncSucceeded, "Sync succeeded", "ok"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	due, err := svc.Store.ListDueDeliveries(ctx, svc.now().Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListDueDeliveries: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("ListDueDeliveries: got %d, want 0", len(due))
	}
}

func TestPublishSuppressesDuringQuietHoursExceptCritical(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, Senders{ChannelWebhook: &FakeSender{}})
	fixedNow := time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC) // 23:00, inside 22:00-07:00
	svc.Now = func() time.Time { return fixedNow }

	ch, err := svc.CreateChannel(ctx, ChannelInput{Name: "A", Type: ChannelWebhook, Enabled: true, Config: ChannelConfig{WebhookURL: "https://a.example/hook"}})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := svc.SetQuietHours(ctx, true, "22:00", "07:00"); err != nil {
		t.Fatalf("SetQuietHours: %v", err)
	}

	if _, err := svc.SetRoute(ctx, EventPoolAboveThreshold, SeverityWarning, []string{ch.ID}); err != nil {
		t.Fatalf("SetRoute (warning): %v", err)
	}
	if err := svc.Publish(ctx, EventPoolAboveThreshold, "Pool above threshold", "90% full"); err != nil {
		t.Fatalf("Publish (warning): %v", err)
	}

	if _, err := svc.SetRoute(ctx, EventDiskOffline, SeverityCritical, []string{ch.ID}); err != nil {
		t.Fatalf("SetRoute (critical): %v", err)
	}
	if err := svc.Publish(ctx, EventDiskOffline, "Disk offline", "sdb is offline"); err != nil {
		t.Fatalf("Publish (critical): %v", err)
	}

	due, err := svc.Store.ListDueDeliveries(ctx, fixedNow.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListDueDeliveries: %v", err)
	}
	if len(due) != 1 || due[0].EventType != EventDiskOffline {
		t.Fatalf("ListDueDeliveries during quiet hours = %+v, want only the critical disk_offline delivery", due)
	}
}

func TestTestChannelReportsSenderError(t *testing.T) {
	ctx := context.Background()
	sender := &FakeSender{Err: fmt.Errorf("connection refused")}
	svc := newTestService(t, Senders{ChannelWebhook: sender})

	ch, err := svc.CreateChannel(ctx, ChannelInput{Name: "A", Type: ChannelWebhook, Enabled: true, Config: ChannelConfig{WebhookURL: "https://a.example/hook"}})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	result, err := svc.TestChannel(ctx, ch.ID)
	if err != nil {
		t.Fatalf("TestChannel: %v", err)
	}
	if result.Success {
		t.Fatal("TestChannel: Success = true, want false")
	}
	if result.Error == "" {
		t.Fatal("TestChannel: Error is empty, want the sender's error")
	}
	if len(sender.Sent) != 1 {
		t.Fatalf("sender.Sent = %d messages, want 1", len(sender.Sent))
	}
}

func TestTestChannelReportsSuccess(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, Senders{ChannelWebhook: &FakeSender{}})

	ch, err := svc.CreateChannel(ctx, ChannelInput{Name: "A", Type: ChannelWebhook, Enabled: true, Config: ChannelConfig{WebhookURL: "https://a.example/hook"}})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	result, err := svc.TestChannel(ctx, ch.ID)
	if err != nil {
		t.Fatalf("TestChannel: %v", err)
	}
	if !result.Success {
		t.Fatalf("TestChannel: Success = false, Error = %q", result.Error)
	}
}

func TestTestChannelNotFound(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, nil)

	if _, err := svc.TestChannel(ctx, "missing"); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("TestChannel(missing): err = %v, want ErrChannelNotFound", err)
	}
}

func TestWithinQuietHoursWraparound(t *testing.T) {
	cases := []struct {
		start, end string
		hour, min  int
		want       bool
	}{
		{"22:00", "07:00", 23, 0, true},
		{"22:00", "07:00", 6, 59, true},
		{"22:00", "07:00", 7, 0, false},
		{"22:00", "07:00", 21, 59, false},
		{"08:00", "17:00", 12, 0, true},
		{"08:00", "17:00", 7, 59, false},
		{"08:00", "08:00", 8, 0, false}, // zero-width window is never active
	}
	for _, c := range cases {
		now := time.Date(2026, 1, 1, c.hour, c.min, 0, 0, time.UTC)
		if got := withinQuietHours(c.start, c.end, now); got != c.want {
			t.Errorf("withinQuietHours(%s, %s, %02d:%02d) = %v, want %v", c.start, c.end, c.hour, c.min, got, c.want)
		}
	}
}

func TestRunDueDeliveriesRetriesThenPermanentlyFails(t *testing.T) {
	ctx := context.Background()
	sender := &FakeSender{Err: fmt.Errorf("timeout")}
	svc := newTestService(t, Senders{ChannelWebhook: sender})
	svc.MaxAttempts = 2
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }

	ch, err := svc.CreateChannel(ctx, ChannelInput{Name: "A", Type: ChannelWebhook, Enabled: true, Config: ChannelConfig{WebhookURL: "https://a.example/hook"}})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := svc.SetRoute(ctx, EventDiskOffline, SeverityCritical, []string{ch.ID}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	if err := svc.Publish(ctx, EventDiskOffline, "Disk offline", "sdb is offline"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	processed, err := svc.RunDueDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("RunDueDeliveries (1st): %v", err)
	}
	if processed != 1 {
		t.Fatalf("RunDueDeliveries (1st): processed = %d, want 1", processed)
	}
	due, err := svc.Store.ListDueDeliveries(ctx, now, 10)
	if err != nil {
		t.Fatalf("ListDueDeliveries: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("ListDueDeliveries right after 1st attempt: got %d, want 0 (still backing off)", len(due))
	}

	svc.Now = func() time.Time { return now.Add(2 * time.Hour) }
	processed, err = svc.RunDueDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("RunDueDeliveries (2nd): %v", err)
	}
	if processed != 1 {
		t.Fatalf("RunDueDeliveries (2nd): processed = %d, want 1", processed)
	}

	dueAfter, err := svc.Store.ListDueDeliveries(ctx, now.Add(3*time.Hour), 10)
	if err != nil {
		t.Fatalf("ListDueDeliveries after max attempts: %v", err)
	}
	if len(dueAfter) != 0 {
		t.Fatalf("ListDueDeliveries after max attempts: got %d, want 0 (permanently failed, not pending)", len(dueAfter))
	}
	if len(sender.Sent) != 2 {
		t.Fatalf("sender.Sent = %d, want 2 attempts", len(sender.Sent))
	}
}

func TestRunDueDeliveriesSucceeds(t *testing.T) {
	ctx := context.Background()
	sender := &FakeSender{}
	svc := newTestService(t, Senders{ChannelWebhook: sender})

	ch, err := svc.CreateChannel(ctx, ChannelInput{Name: "A", Type: ChannelWebhook, Enabled: true, Config: ChannelConfig{WebhookURL: "https://a.example/hook"}})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := svc.SetRoute(ctx, EventDiskOffline, SeverityCritical, []string{ch.ID}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	if err := svc.Publish(ctx, EventDiskOffline, "Disk offline", "sdb is offline"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	processed, err := svc.RunDueDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("RunDueDeliveries: %v", err)
	}
	if processed != 1 {
		t.Fatalf("RunDueDeliveries: processed = %d, want 1", processed)
	}
	if len(sender.Sent) != 1 {
		t.Fatalf("sender.Sent = %d, want 1", len(sender.Sent))
	}
	if sender.Sent[0].Title != "Disk offline" {
		t.Fatalf("sender.Sent[0].Title = %q, want %q", sender.Sent[0].Title, "Disk offline")
	}

	due, err := svc.Store.ListDueDeliveries(ctx, svc.now().Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListDueDeliveries: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("ListDueDeliveries after success: got %d, want 0", len(due))
	}
}

// TestRunDueDeliveriesBoundedByUnresponsiveSMTPPeer proves a hung SMTP
// peer (a listener that accepts and never speaks) doesn't stall
// RunDueDeliveries past ctx's own deadline — the real EmailSender, not
// FakeSender, so this exercises defaultSendMail's actual dial/deadline
// handling, not a fake that already returns instantly.
func TestRunDueDeliveriesBoundedByUnresponsiveSMTPPeer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		time.Sleep(5 * time.Second) // accept, then never speak
	}()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi: %v", err)
	}

	// ctx deliberately carries no deadline, matching how the daemon's job
	// system actually calls RunDueDeliveries: the bound comes from
	// EmailSender's own Timeout (lowered here so the test doesn't wait
	// out the real production default), not from an externally imposed
	// context deadline that would also cut off every other delivery in
	// the same batch.
	ctx := context.Background()
	svc := newTestService(t, Senders{ChannelEmail: &EmailSender{Timeout: 300 * time.Millisecond}})
	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "Ops email", Type: ChannelEmail, Enabled: true,
		Config: ChannelConfig{EmailHost: host, EmailPort: port, EmailFrom: "a@example.com", EmailTo: []string{"b@example.com"}},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := svc.SetRoute(ctx, EventDiskOffline, SeverityCritical, []string{ch.ID}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	if err := svc.Publish(ctx, EventDiskOffline, "Disk offline", "sdb is offline"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	start := time.Now()
	processed, err := svc.RunDueDeliveries(ctx, 10)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("RunDueDeliveries: %v", err)
	}
	if processed != 1 {
		t.Fatalf("RunDueDeliveries: processed = %d, want 1 (the hung send is still an attempted, recorded delivery)", processed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("RunDueDeliveries took %v against an unresponsive SMTP peer, want it bounded by EmailSender.Timeout", elapsed)
	}

	due, err := svc.Store.ListDueDeliveries(ctx, svc.now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("ListDueDeliveries: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("ListDueDeliveries: got %d, want the timed-out delivery still pending for retry", len(due))
	}
}
