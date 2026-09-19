package notify

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(newTestDB(t))
}

func TestChannelCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	ch := &Channel{
		ID:      "chan-1",
		Name:    "Ops email",
		Type:    ChannelEmail,
		Enabled: true,
		Config: ChannelConfig{
			EmailHost: "smtp.example.com",
			EmailPort: 587,
			EmailFrom: "hoserva@example.com",
			EmailTo:   []string{"ops@example.com"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateChannel(ctx, ch, []byte("ciphertext")); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	got, err := s.GetChannel(ctx, "chan-1")
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if got.Name != "Ops email" || got.Type != ChannelEmail || !got.HasSecret {
		t.Fatalf("GetChannel: got %+v", got)
	}
	if got.Config.EmailHost != "smtp.example.com" || got.Config.EmailPort != 587 {
		t.Fatalf("GetChannel: config = %+v", got.Config)
	}

	secret, err := s.getChannelSecretCiphertext(ctx, "chan-1")
	if err != nil {
		t.Fatalf("getChannelSecretCiphertext: %v", err)
	}
	if string(secret) != "ciphertext" {
		t.Fatalf("getChannelSecretCiphertext = %q, want %q", secret, "ciphertext")
	}

	list, err := s.ListChannels(ctx)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListChannels: got %d channels, want 1", len(list))
	}

	got.Name = "Renamed"
	got.Enabled = false
	got.UpdatedAt = now.Add(time.Hour)
	if err := s.UpdateChannel(ctx, got, nil); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	updated, err := s.GetChannel(ctx, "chan-1")
	if err != nil {
		t.Fatalf("GetChannel after update: %v", err)
	}
	if updated.Name != "Renamed" || updated.Enabled || updated.HasSecret {
		t.Fatalf("GetChannel after update: got %+v, want secret cleared", updated)
	}

	if err := s.DeleteChannel(ctx, "chan-1"); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	if _, err := s.GetChannel(ctx, "chan-1"); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("GetChannel after delete: err = %v, want ErrChannelNotFound", err)
	}
}

func TestUpdateChannelNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	ch := &Channel{ID: "missing", Name: "x", Type: ChannelWebhook, UpdatedAt: time.Now()}
	if err := s.UpdateChannel(ctx, ch, nil); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("UpdateChannel(missing): err = %v, want ErrChannelNotFound", err)
	}
}

// TestDeleteChannelCascadesRoutesAndDeliveries proves the delete works at
// the database level, not just on whichever connection ran it: it reads
// back through a second, independent connection to the same file (opened
// the same way cmd/hoservad opens its database, via store.DSN), so the
// result can't be an artifact of this test happening to reuse one
// connection or of foreign_keys being on for that one connection only.
func TestDeleteChannelCascadesRoutesAndDeliveries(t *testing.T) {
	ctx := context.Background()
	dbPath := newTestDBPath(t)
	s := NewStore(openTestDB(t, dbPath))
	now := time.Now()

	ch := &Channel{ID: "chan-1", Name: "x", Type: ChannelWebhook, Enabled: true, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateChannel(ctx, ch, nil); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if err := s.SetRoute(ctx, EventDiskOffline, SeverityCritical, []string{"chan-1"}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	if err := s.CreateDelivery(ctx, &Delivery{
		ID: "d1", ChannelID: "chan-1", EventType: EventDiskOffline, Severity: SeverityCritical,
		Title: "t", Message: "m", Status: DeliveryPending, CreatedAt: now, NextAttemptAt: now,
	}); err != nil {
		t.Fatalf("CreateDelivery: %v", err)
	}

	if err := s.DeleteChannel(ctx, "chan-1"); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}

	routing, err := s.GetRouting(ctx)
	if err != nil {
		t.Fatalf("GetRouting: %v", err)
	}
	for _, entry := range routing {
		if entry.EventType == EventDiskOffline && len(entry.ChannelIDs) != 0 {
			t.Fatalf("GetRouting: disk_offline still routed to %v after channel deletion", entry.ChannelIDs)
		}
	}

	due, err := s.ListDueDeliveries(ctx, now.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListDueDeliveries: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("ListDueDeliveries: got %d, want 0 after channel deletion cascaded", len(due))
	}

	second := openTestDB(t, dbPath)
	var routeCount, deliveryCount int
	if err := second.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_routes WHERE channel_id = ?`, "chan-1").Scan(&routeCount); err != nil {
		t.Fatalf("counting notify_routes on second connection: %v", err)
	}
	if routeCount != 0 {
		t.Fatalf("notify_routes: %d rows for chan-1 remain on a second connection after delete, want 0", routeCount)
	}
	if err := second.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_deliveries WHERE channel_id = ?`, "chan-1").Scan(&deliveryCount); err != nil {
		t.Fatalf("counting notify_deliveries on second connection: %v", err)
	}
	if deliveryCount != 0 {
		t.Fatalf("notify_deliveries: %d rows for chan-1 remain on a second connection after delete, want 0", deliveryCount)
	}
}

func TestGetRoutingCoversEveryCatalogEntryWithDefaults(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	routing, err := s.GetRouting(ctx)
	if err != nil {
		t.Fatalf("GetRouting: %v", err)
	}
	if len(routing) != len(EventCatalog) {
		t.Fatalf("GetRouting: got %d entries, want %d (one per catalog event)", len(routing), len(EventCatalog))
	}
	for i, entry := range routing {
		if entry.EventType != EventCatalog[i] {
			t.Fatalf("GetRouting[%d]: event = %s, want %s (catalog order)", i, entry.EventType, EventCatalog[i])
		}
		want, _ := DefaultSeverity(entry.EventType)
		if entry.Severity != want {
			t.Fatalf("GetRouting[%d] (%s): severity = %s, want default %s", i, entry.EventType, entry.Severity, want)
		}
		if len(entry.ChannelIDs) != 0 {
			t.Fatalf("GetRouting[%d] (%s): channelIDs = %v, want none before any route is set", i, entry.EventType, entry.ChannelIDs)
		}
	}
}

func TestSetRouteReplacesWholeRow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()

	for _, id := range []string{"chan-1", "chan-2"} {
		ch := &Channel{ID: id, Name: id, Type: ChannelWebhook, Enabled: true, CreatedAt: now, UpdatedAt: now}
		if err := s.CreateChannel(ctx, ch, nil); err != nil {
			t.Fatalf("CreateChannel(%s): %v", id, err)
		}
	}

	if err := s.SetRoute(ctx, EventSyncFailed, SeverityCritical, []string{"chan-1", "chan-2"}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	sev, err := s.GetEffectiveSeverity(ctx, EventSyncFailed)
	if err != nil {
		t.Fatalf("GetEffectiveSeverity: %v", err)
	}
	if sev != SeverityCritical {
		t.Fatalf("GetEffectiveSeverity = %s, want critical", sev)
	}

	// Replacing with a smaller channel list drops the one no longer named.
	if err := s.SetRoute(ctx, EventSyncFailed, SeverityWarning, []string{"chan-2"}); err != nil {
		t.Fatalf("SetRoute (replace): %v", err)
	}
	channels, err := s.ChannelsForEvent(ctx, EventSyncFailed)
	if err != nil {
		t.Fatalf("ChannelsForEvent: %v", err)
	}
	if len(channels) != 1 || channels[0].ID != "chan-2" {
		t.Fatalf("ChannelsForEvent after replace = %+v, want only chan-2", channels)
	}
	sev, err = s.GetEffectiveSeverity(ctx, EventSyncFailed)
	if err != nil {
		t.Fatalf("GetEffectiveSeverity: %v", err)
	}
	if sev != SeverityWarning {
		t.Fatalf("GetEffectiveSeverity after replace = %s, want warning", sev)
	}
}

func TestChannelsForEventExcludesDisabledChannels(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()

	ch := &Channel{ID: "chan-1", Name: "x", Type: ChannelWebhook, Enabled: false, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateChannel(ctx, ch, nil); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if err := s.SetRoute(ctx, EventDiskOffline, SeverityCritical, []string{"chan-1"}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	channels, err := s.ChannelsForEvent(ctx, EventDiskOffline)
	if err != nil {
		t.Fatalf("ChannelsForEvent: %v", err)
	}
	if len(channels) != 0 {
		t.Fatalf("ChannelsForEvent: got %d channels, want 0 (disabled channel excluded)", len(channels))
	}
}

func TestQuietHoursDefaultsToDisabled(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	qh, err := s.GetQuietHours(ctx)
	if err != nil {
		t.Fatalf("GetQuietHours: %v", err)
	}
	if qh.Enabled {
		t.Fatalf("GetQuietHours (never set): Enabled = true, want false")
	}

	now := time.Now()
	if err := s.SetQuietHours(ctx, QuietHours{Enabled: true, StartTime: "22:00", EndTime: "07:00", UpdatedAt: now}); err != nil {
		t.Fatalf("SetQuietHours: %v", err)
	}
	qh, err = s.GetQuietHours(ctx)
	if err != nil {
		t.Fatalf("GetQuietHours after set: %v", err)
	}
	if !qh.Enabled || qh.StartTime != "22:00" || qh.EndTime != "07:00" {
		t.Fatalf("GetQuietHours after set = %+v", qh)
	}
}

func TestListDueDeliveriesRespectsNextAttemptAt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()

	ch := &Channel{ID: "chan-1", Name: "x", Type: ChannelWebhook, Enabled: true, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateChannel(ctx, ch, nil); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	due := &Delivery{ID: "d1", ChannelID: "chan-1", EventType: EventDiskOffline, Severity: SeverityCritical, Title: "t", Message: "m", Status: DeliveryPending, CreatedAt: now, NextAttemptAt: now.Add(-time.Minute)}
	future := &Delivery{ID: "d2", ChannelID: "chan-1", EventType: EventDiskOffline, Severity: SeverityCritical, Title: "t", Message: "m", Status: DeliveryPending, CreatedAt: now, NextAttemptAt: now.Add(time.Hour)}
	if err := s.CreateDelivery(ctx, due); err != nil {
		t.Fatalf("CreateDelivery(due): %v", err)
	}
	if err := s.CreateDelivery(ctx, future); err != nil {
		t.Fatalf("CreateDelivery(future): %v", err)
	}

	got, err := s.ListDueDeliveries(ctx, now, 10)
	if err != nil {
		t.Fatalf("ListDueDeliveries: %v", err)
	}
	if len(got) != 1 || got[0].ID != "d1" {
		t.Fatalf("ListDueDeliveries = %+v, want only d1", got)
	}
}
