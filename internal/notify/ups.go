package notify

import "context"

// PublishUPSOnBattery is doc 03 §8.3's "UPS on battery" event source
// (Q77, doc 02 §6): job.UPSController calls this the moment NUT's own
// upsmon reports the UPS switched to battery power — the mover pauses
// and scheduled syncs hold at the same time, in internal/job, not here.
func (s *Service) PublishUPSOnBattery(ctx context.Context) error {
	return s.Publish(ctx, EventUPSOnBattery,
		"UPS on battery power",
		"The UPS reported a loss of utility power. The mover is paused and scheduled syncs are held until power returns.",
	)
}

// PublishUPSBatteryLow is doc 03 §8.3's "UPS battery low" event source
// (Q77, doc 02 §6): job.UPSController calls this when NUT's own upsmon
// reports the battery low, or the configured on-battery runtime elapsed
// — at the same moment it checkpoints every running Array-write job,
// marks a running sync interrupted, and runs the clean shutdown
// sequence.
func (s *Service) PublishUPSBatteryLow(ctx context.Context) error {
	return s.Publish(ctx, EventUPSBatteryLow,
		"UPS battery low — shutting down",
		"The UPS battery is low. Running jobs are being checkpointed and the system is shutting down cleanly.",
	)
}
