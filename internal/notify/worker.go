package notify

import (
	"context"
	"fmt"
	"time"
)

// DefaultDeliveryBatchSize bounds how many due deliveries RunDueDeliveries
// attempts per call.
const DefaultDeliveryBatchSize = 50

// retryBackoff is how long a failed delivery waits before its next
// attempt, indexed by attempts-so-far (1st failure waits the first
// entry, and so on) — capped at the last entry rather than growing
// without bound.
var retryBackoff = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
}

func backoffFor(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > len(retryBackoff) {
		attempts = len(retryBackoff)
	}
	return retryBackoff[attempts-1]
}

// RunDueDeliveries attempts every notify_deliveries row currently due (up
// to limit), and persists each one's outcome before returning — a
// delivery failure is never silently dropped (#35): it is retried, up to
// MaxAttempts, with the schedule above, and every attempt's result is
// both logged and written back to its row, whether it ultimately
// succeeds, keeps retrying, or is marked permanently failed.
//
// Nothing calls this on a timer inside this package — CLAUDE.md's "never
// walk a data disk on a timer" doesn't apply here (this table lives in
// the central SQLite database, never a data disk), but this is still
// deliberately a plain method a caller drives, exactly like
// internal/job.Scheduler's own methods, rather than a goroutine this
// package starts itself.
func (s *Service) RunDueDeliveries(ctx context.Context, limit int) (processed int, err error) {
	if limit <= 0 {
		limit = DefaultDeliveryBatchSize
	}
	now := s.now()
	due, err := s.Store.ListDueDeliveries(ctx, now, limit)
	if err != nil {
		return 0, fmt.Errorf("notify: listing due deliveries: %w", err)
	}

	for _, d := range due {
		if err := s.attemptDelivery(ctx, d, now); err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

// attemptDelivery sends one delivery and persists its outcome. A returned
// error is an infrastructure failure (the store itself); a channel-side
// or send failure is handled and recorded here, never propagated.
func (s *Service) attemptDelivery(ctx context.Context, d *Delivery, now time.Time) error {
	ch, err := s.Store.GetChannel(ctx, d.ChannelID)
	if err != nil {
		d.Attempts++
		d.Status = DeliveryFailed
		d.LastError = fmt.Sprintf("channel no longer exists: %v", err)
		s.log("delivery %s: channel %s no longer exists, giving up: %v", d.ID, d.ChannelID, err)
		return s.Store.UpdateDeliveryAttempt(ctx, d)
	}
	if !ch.Enabled {
		d.Status = DeliverySuppressed
		d.LastError = "channel is disabled"
		s.log("delivery %s: channel %s is disabled, suppressing", d.ID, ch.ID)
		return s.Store.UpdateDeliveryAttempt(ctx, d)
	}

	secret, err := s.decryptChannelSecret(ctx, ch.ID)
	if err != nil {
		d.Attempts++
		d.Status = DeliveryFailed
		d.LastError = fmt.Sprintf("decrypting channel credential: %v", err)
		s.log("delivery %s: decrypting channel %s credential failed, giving up: %v", d.ID, ch.ID, err)
		return s.Store.UpdateDeliveryAttempt(ctx, d)
	}

	sender, ok := s.Senders[ch.Type]
	if !ok {
		d.Attempts++
		d.Status = DeliveryFailed
		d.LastError = fmt.Sprintf("no sender registered for channel type %q", ch.Type)
		s.log("delivery %s: %s", d.ID, d.LastError)
		return s.Store.UpdateDeliveryAttempt(ctx, d)
	}

	sendErr := sender.Send(ctx, ch.Config, secret, Message{
		EventType: d.EventType,
		Severity:  d.Severity,
		Title:     d.Title,
		Body:      d.Message,
	})
	d.Attempts++
	if sendErr == nil {
		d.Status = DeliveryDelivered
		d.LastError = ""
		deliveredAt := now
		d.DeliveredAt = &deliveredAt
		s.log("delivery %s to channel %s (%s) succeeded on attempt %d", d.ID, ch.ID, ch.Type, d.Attempts)
		return s.Store.UpdateDeliveryAttempt(ctx, d)
	}

	d.LastError = sendErr.Error()
	if d.Attempts >= s.MaxAttempts {
		d.Status = DeliveryFailed
		s.log("delivery %s to channel %s (%s) permanently failed after %d attempts: %v", d.ID, ch.ID, ch.Type, d.Attempts, sendErr)
	} else {
		d.Status = DeliveryPending
		d.NextAttemptAt = now.Add(backoffFor(d.Attempts))
		s.log("delivery %s to channel %s (%s) failed (attempt %d/%d), retrying at %s: %v", d.ID, ch.ID, ch.Type, d.Attempts, s.MaxAttempts, d.NextAttemptAt.Format(time.RFC3339), sendErr)
	}
	return s.Store.UpdateDeliveryAttempt(ctx, d)
}
