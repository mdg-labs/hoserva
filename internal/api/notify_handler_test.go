package api_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newNotifyTestHandler wires a Handler with a real, migrated database and
// a Service backed by notify.FakeSecretCipher and a FakeSender per
// channel type — no real network or encryption key ever touched.
func newNotifyTestHandler(t *testing.T) (*api.Handler, *notify.Service, map[notify.ChannelType]*notify.FakeSender) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "notify-handler-test.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	senders := map[notify.ChannelType]*notify.FakeSender{
		notify.ChannelEmail:   {},
		notify.ChannelGotify:  {},
		notify.ChannelNtfy:    {},
		notify.ChannelDiscord: {},
		notify.ChannelWebhook: {},
	}
	svc := notify.NewService(notify.NewStore(db), notify.FakeSecretCipher{}, notify.Senders{
		notify.ChannelEmail:   senders[notify.ChannelEmail],
		notify.ChannelGotify:  senders[notify.ChannelGotify],
		notify.ChannelNtfy:    senders[notify.ChannelNtfy],
		notify.ChannelDiscord: senders[notify.ChannelDiscord],
		notify.ChannelWebhook: senders[notify.ChannelWebhook],
	})
	svc.Log = func(string, ...any) {}

	return &api.Handler{Notify: svc}, svc, senders
}

func createWebhookChannel(t *testing.T, h *api.Handler, name string) *apiv1.NotificationChannel {
	t.Helper()
	ch, err := h.CreateNotificationChannel(context.Background(), &apiv1.CreateNotificationChannelRequest{
		Name:       name,
		Type:       apiv1.NotificationChannelTypeWebhook,
		Enabled:    true,
		WebhookUrl: apiv1.NewOptString("https://example.com/hook"),
	})
	if err != nil {
		t.Fatalf("CreateNotificationChannel: %v", err)
	}
	return ch
}

func TestHandlerCreateAndGetNotificationChannel(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newNotifyTestHandler(t)

	ch := createWebhookChannel(t, h, "Ops webhook")
	if ch.HasSecret {
		t.Error("HasSecret should be false when no secret was supplied")
	}

	got, err := h.GetNotificationChannel(ctx, apiv1.GetNotificationChannelParams{ChannelId: ch.ID})
	if err != nil {
		t.Fatalf("GetNotificationChannel: %v", err)
	}
	if got.Name != "Ops webhook" || got.WebhookUrl.Value != "https://example.com/hook" {
		t.Fatalf("GetNotificationChannel = %+v", got)
	}
}

// TestHandlerCreateNotificationChannelWithSecretNeverEchoesIt confirms the
// API response never carries a supplied credential (Q28) — only
// HasSecret reports one is configured. The encryption itself, and that
// the stored value round-trips back to the original plaintext, is
// covered by internal/notify's own tests (TestCreateChannelEncryptsSecret).
func TestHandlerCreateNotificationChannelWithSecretNeverEchoesIt(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newNotifyTestHandler(t)

	ch, err := h.CreateNotificationChannel(ctx, &apiv1.CreateNotificationChannelRequest{
		Name:    "Discord",
		Type:    apiv1.NotificationChannelTypeDiscord,
		Enabled: true,
		Secret:  apiv1.NewOptString("https://discord.example/webhooks/1/abc"),
	})
	if err != nil {
		t.Fatalf("CreateNotificationChannel: %v", err)
	}
	if !ch.HasSecret {
		t.Fatal("HasSecret should be true once a secret is supplied")
	}
}

func TestHandlerListNotificationChannels(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newNotifyTestHandler(t)

	createWebhookChannel(t, h, "A")
	createWebhookChannel(t, h, "B")

	list, err := h.ListNotificationChannels(ctx)
	if err != nil {
		t.Fatalf("ListNotificationChannels: %v", err)
	}
	if len(list.Channels) != 2 {
		t.Fatalf("ListNotificationChannels: got %d, want 2", len(list.Channels))
	}
}

func TestHandlerUpdateNotificationChannelSecretTriState(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newNotifyTestHandler(t)

	ch, err := h.CreateNotificationChannel(ctx, &apiv1.CreateNotificationChannelRequest{
		Name: "Discord", Type: apiv1.NotificationChannelTypeDiscord, Enabled: true,
		Secret: apiv1.NewOptString("https://discord.example/webhooks/1/abc"),
	})
	if err != nil {
		t.Fatalf("CreateNotificationChannel: %v", err)
	}

	// Omitted secret keeps it.
	updated, err := h.UpdateNotificationChannel(ctx, &apiv1.UpdateNotificationChannelRequest{
		Name: "Discord renamed", Type: apiv1.NotificationChannelTypeDiscord, Enabled: true,
	}, apiv1.UpdateNotificationChannelParams{ChannelId: ch.ID})
	if err != nil {
		t.Fatalf("UpdateNotificationChannel (keep): %v", err)
	}
	if !updated.HasSecret {
		t.Fatal("HasSecret should stay true when secret is omitted from the update")
	}

	// Explicit null clears it.
	req := &apiv1.UpdateNotificationChannelRequest{Name: "Discord renamed", Type: apiv1.NotificationChannelTypeDiscord, Enabled: true}
	req.Secret.SetToNull()
	updated, err = h.UpdateNotificationChannel(ctx, req, apiv1.UpdateNotificationChannelParams{ChannelId: ch.ID})
	if err != nil {
		t.Fatalf("UpdateNotificationChannel (clear): %v", err)
	}
	if updated.HasSecret {
		t.Fatal("HasSecret should be false after an explicit null secret")
	}
}

func TestHandlerDeleteNotificationChannel(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newNotifyTestHandler(t)

	ch := createWebhookChannel(t, h, "A")
	if err := h.DeleteNotificationChannel(ctx, apiv1.DeleteNotificationChannelParams{ChannelId: ch.ID}); err != nil {
		t.Fatalf("DeleteNotificationChannel: %v", err)
	}
	if _, err := h.GetNotificationChannel(ctx, apiv1.GetNotificationChannelParams{ChannelId: ch.ID}); err == nil {
		t.Fatal("GetNotificationChannel: expected an error after delete")
	} else if apiErr, ok := errAsAPIError(err); !ok || apiErr.Response.Code != "notification_channel_not_found" {
		t.Fatalf("GetNotificationChannel error = %v, want notification_channel_not_found", err)
	}
}

func TestHandlerSendTestNotificationReportsFailure(t *testing.T) {
	ctx := context.Background()
	h, _, senders := newNotifyTestHandler(t)
	senders[notify.ChannelWebhook].Err = errTestSendFailure

	ch := createWebhookChannel(t, h, "A")
	result, err := h.SendTestNotification(ctx, apiv1.SendTestNotificationParams{ChannelId: ch.ID})
	if err != nil {
		t.Fatalf("SendTestNotification: %v", err)
	}
	if result.Success {
		t.Fatal("Success should be false when the sender errors")
	}
	errVal, ok := result.Error.Get()
	if !ok || errVal == "" {
		t.Fatal("Error should be set when Success is false")
	}
}

func TestHandlerSendTestNotificationReportsSuccess(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newNotifyTestHandler(t)

	ch := createWebhookChannel(t, h, "A")
	result, err := h.SendTestNotification(ctx, apiv1.SendTestNotificationParams{ChannelId: ch.ID})
	if err != nil {
		t.Fatalf("SendTestNotification: %v", err)
	}
	if !result.Success {
		t.Fatal("Success should be true")
	}
}

func TestHandlerNotificationRoutingCoversWholeCatalog(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newNotifyTestHandler(t)

	routing, err := h.GetNotificationRouting(ctx)
	if err != nil {
		t.Fatalf("GetNotificationRouting: %v", err)
	}
	if len(routing.Routing) != len(notify.EventCatalog) {
		t.Fatalf("GetNotificationRouting: got %d entries, want %d", len(routing.Routing), len(notify.EventCatalog))
	}
}

func TestHandlerUpdateNotificationRoute(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newNotifyTestHandler(t)

	ch := createWebhookChannel(t, h, "A")
	entry, err := h.UpdateNotificationRoute(ctx, &apiv1.UpdateNotificationRouteRequest{
		Severity:   apiv1.NotificationLevelCritical,
		ChannelIds: []uuid.UUID{ch.ID},
	}, apiv1.UpdateNotificationRouteParams{EventType: apiv1.NotificationEventTypeDiskOffline})
	if err != nil {
		t.Fatalf("UpdateNotificationRoute: %v", err)
	}
	if entry.Severity != apiv1.NotificationLevelCritical || len(entry.ChannelIds) != 1 {
		t.Fatalf("UpdateNotificationRoute result = %+v", entry)
	}
}

func TestHandlerUpdateNotificationRouteRejectsUnknownChannel(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newNotifyTestHandler(t)

	_, err := h.UpdateNotificationRoute(ctx, &apiv1.UpdateNotificationRouteRequest{
		Severity:   apiv1.NotificationLevelCritical,
		ChannelIds: []uuid.UUID{uuid.New()},
	}, apiv1.UpdateNotificationRouteParams{EventType: apiv1.NotificationEventTypeDiskOffline})
	if err == nil {
		t.Fatal("expected an error for an unknown channel id")
	}
	if apiErr, ok := errAsAPIError(err); !ok || apiErr.Response.Code != "notification_unknown_channel" {
		t.Fatalf("error = %v, want notification_unknown_channel", err)
	}
}

func TestHandlerQuietHours(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newNotifyTestHandler(t)

	got, err := h.GetQuietHours(ctx)
	if err != nil {
		t.Fatalf("GetQuietHours: %v", err)
	}
	if got.Enabled {
		t.Fatal("Enabled should default to false")
	}
	if !got.CriticalAlwaysDelivers {
		t.Fatal("CriticalAlwaysDelivers must always be true")
	}

	updated, err := h.UpdateQuietHours(ctx, &apiv1.UpdateQuietHoursRequest{Enabled: true, Start: "22:00", End: "07:00"})
	if err != nil {
		t.Fatalf("UpdateQuietHours: %v", err)
	}
	if !updated.Enabled || updated.Start != "22:00" || updated.End != "07:00" {
		t.Fatalf("UpdateQuietHours result = %+v", updated)
	}
	if !updated.CriticalAlwaysDelivers {
		t.Fatal("CriticalAlwaysDelivers must always be true, even after an update")
	}
}

func TestHandlerUpdateQuietHoursRejectsInvalidTime(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newNotifyTestHandler(t)

	_, err := h.UpdateQuietHours(ctx, &apiv1.UpdateQuietHoursRequest{Enabled: true, Start: "25:00", End: "07:00"})
	if err == nil {
		t.Fatal("expected an error for an invalid start time")
	}
	if apiErr, ok := errAsAPIError(err); !ok || apiErr.Response.Code != "notification_invalid_input" {
		t.Fatalf("error = %v, want notification_invalid_input", err)
	}
}

var errTestSendFailure = &fakeSendError{"connection refused"}

type fakeSendError struct{ msg string }

func (e *fakeSendError) Error() string { return e.msg }

// errAsAPIError renders err through Handler.NewError, exactly as the
// generated router would before writing a response — the only way a test
// outside the api package can see the classified code without exporting
// apiError itself.
func errAsAPIError(err error) (*apiv1.ErrorStatusCode, bool) {
	var h api.Handler
	got := h.NewError(context.Background(), err)
	if got == nil {
		return nil, false
	}
	return got, true
}
