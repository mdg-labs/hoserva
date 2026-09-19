package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newNotifyTestDB applies every real, embedded schema migration (D16), so
// this test exercises the notify_* tables exactly as internal/store.Runner
// leaves them at daemon startup — never a hand-built CREATE TABLE.
func newNotifyTestDB(t *testing.T) *sql.DB {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "hoservad-notify-test.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return db
}

// TestRunNotifyDeliveryLoopSendsQueuedDelivery proves #165's own wiring,
// not #35's already-tested retry logic: a delivery queued through the
// exact same *notify.Service construction main.go's run() wires up
// actually gets sent once runNotifyDeliveryLoop ticks, and the loop
// itself stops cleanly when ctx is cancelled.
func TestRunNotifyDeliveryLoopSendsQueuedDelivery(t *testing.T) {
	db := newNotifyTestDB(t)
	notifyStore := notify.NewStore(db)
	sender := &notify.FakeSender{}
	svc := notify.NewService(notifyStore, notify.FakeSecretCipher{}, notify.Senders{notify.ChannelWebhook: sender})
	svc.Log = func(string, ...any) {}

	ctx := context.Background()
	ch, err := svc.CreateChannel(ctx, notify.ChannelInput{
		Name: "Ops webhook", Type: notify.ChannelWebhook, Enabled: true,
		Config: notify.ChannelConfig{WebhookURL: "https://example.invalid/hook"},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := svc.SetRoute(ctx, notify.EventDiskOffline, notify.SeverityCritical, []string{ch.ID}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	if err := svc.Publish(ctx, notify.EventDiskOffline, "Disk offline", "sdb is offline"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		runNotifyDeliveryLoop(loopCtx, svc, 10*time.Millisecond, notifyDeliveryBatchLimit)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for len(sender.Sent) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("timed out waiting for runNotifyDeliveryLoop to send the queued delivery")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := sender.Sent[0].Title; got != "Disk offline" {
		t.Fatalf("sender.Sent[0].Title = %q, want %q", got, "Disk offline")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runNotifyDeliveryLoop did not stop after ctx cancellation")
	}
}
