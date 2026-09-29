package main

import (
	"context"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/notify"
)

// stalePublisher is the one notify.Service method the stale-destination
// alert needs; tests use a fake so this package needn't build a real one.
type stalePublisher interface {
	Publish(ctx context.Context, event notify.EventType, title, message string) error
}

// staleDestinationChecker turns backup.Service's stale-destination check
// into notify.EventBackupDestinationStale alerts. It runs on
// scheduleRunner's existing tick and reads stored timestamps only — it
// never touches a destination (Q13).
type staleDestinationChecker struct {
	svc       *backup.Service
	publisher stalePublisher
	now       func() time.Time
}

func (c *staleDestinationChecker) CheckStale(ctx context.Context) error {
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	return c.svc.CheckStaleDestinations(ctx, now().UTC(), func(ctx context.Context, name string, last *time.Time) error {
		title := fmt.Sprintf("Backup destination %q has no recent backup", name)
		message := fmt.Sprintf("No config backup has been written to %q for over %d hours", name, int(backup.StaleAfter.Hours()))
		if last != nil {
			message += fmt.Sprintf(" (the last one was at %s)", last.UTC().Format(time.RFC3339))
		}
		return c.publisher.Publish(ctx, notify.EventBackupDestinationStale, title, message+".")
	})
}
