package job

import "time"

// ScheduledWindow is one job's next scheduled run: the class and resource
// scope it would run under, and the time span it is expected to occupy. It
// carries no cron expression or persistence of its own — internal/api's
// schedule service computes each separately-scheduled job's next-run
// instant and expected duration and builds one of these per job.
// DetectConflict is the reusable check "would these two actually collide"
// that the nightly chain itself no longer needs (its own steps are already
// serialized, doc 02 §2/§3, Q30) but a job scheduled outside it does (doc 03
// §8.4: "conflict detection covers jobs scheduled outside the chain — e.g.
// appdata backup overlapping the chain, or two heavy jobs at once").
type ScheduledWindow struct {
	Class       Class
	ResourceIDs []string
	Start       time.Time
	Duration    time.Duration
}

func (w ScheduledWindow) end() time.Time { return w.Start.Add(w.Duration) }

// DetectConflict reports whether a and b would violate doc 01 §4's
// mutually exclusive job classes if both ran as scheduled: their time spans
// must actually overlap, and their classes and resource scopes must be
// ones Scheduler itself would refuse to run at the same time (conflicts,
// exclusion.go) — the identical rule, applied to two scheduled windows
// ahead of time instead of to two running jobs at dispatch, so the
// settings page can warn before saving a schedule rather than after the two
// jobs collide at 2am.
func DetectConflict(a, b ScheduledWindow) bool {
	if !a.Start.Before(b.end()) || !b.Start.Before(a.end()) {
		return false
	}
	return conflicts(a.Class, b.Class, a.ResourceIDs, b.ResourceIDs)
}
