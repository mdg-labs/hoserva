package job

import (
	"fmt"
	"strings"
	"time"
)

const (
	DefaultChainStartTime    = "02:00"
	DefaultWeeklyScrubDay    = 0 // Sunday
	MaintenanceChainJobID    = "maintenance_chain"
	scheduleTimePatternParts = 2
)

// Frequency is how often a separately scheduled job runs (doc 03 §8.4).
type Frequency string

const (
	FrequencyDaily   Frequency = "daily"
	FrequencyWeekly  Frequency = "weekly"
	FrequencyMonthly Frequency = "monthly"
)

// ChainSettings is the persisted nightly maintenance chain configuration
// (Q30, doc 03 §8.4).
type ChainSettings struct {
	StartTime      string
	WeeklyScrubDay int
	Enabled        map[Step]bool
}

// OtherJobSettings is one separately scheduled recurring job.
type OtherJobSettings struct {
	ID        string
	Enabled   bool
	Frequency Frequency
	Time      string
}

// ScheduleConflict names two schedules whose next-run windows would
// violate doc 01 §4 if both ran as scheduled.
type ScheduleConflict struct {
	JobA string
	JobB string
}

// NamedWindow is one schedule's next expected run span for conflict
// detection.
type NamedWindow struct {
	JobID  string
	Window ScheduledWindow
}

var defaultChainEnabled = map[Step]bool{
	StepMover:        true,
	StepDiffGuard:    true,
	StepSync:         true,
	StepScrub:        true,
	StepConfigBackup: true,
}

var defaultOtherJobs = []OtherJobSettings{
	{ID: "smart_self_test", Enabled: true, Frequency: FrequencyWeekly, Time: "03:00"},
	{ID: "appdata_backup", Enabled: false, Frequency: FrequencyDaily, Time: "04:00"},
	{ID: "restore_drill", Enabled: false, Frequency: FrequencyMonthly, Time: "05:00"},
	{ID: "container_update_check", Enabled: true, Frequency: FrequencyDaily, Time: "06:00"},
}

// stepDurationEstimates are conservative planning windows for conflict
// detection — not runtime predictions.
var stepDurationEstimates = map[Step]time.Duration{
	StepMover:        time.Hour,
	StepDiffGuard:    15 * time.Minute,
	StepSync:         2 * time.Hour,
	StepScrub:        4 * time.Hour,
	StepConfigBackup: 10 * time.Minute,
}

var otherJobDurationEstimates = map[string]time.Duration{
	"smart_self_test":        2 * time.Hour,
	"appdata_backup":         3 * time.Hour,
	"restore_drill":          time.Hour,
	"container_update_check": 30 * time.Minute,
}

var otherJobClasses = map[string]Class{
	"smart_self_test":        ClassService,
	"appdata_backup":         ClassService,
	"restore_drill":          ClassService,
	"container_update_check": ClassService,
}

// DefaultChainSettings returns Q30's defaults when nothing is persisted yet.
func DefaultChainSettings() ChainSettings {
	enabled := make(map[Step]bool, len(defaultChainEnabled))
	for step, on := range defaultChainEnabled {
		enabled[step] = on
	}
	return ChainSettings{
		StartTime:      DefaultChainStartTime,
		WeeklyScrubDay: DefaultWeeklyScrubDay,
		Enabled:        enabled,
	}
}

// DefaultOtherJobs returns doc 03 §8.4's default separately scheduled jobs.
func DefaultOtherJobs() []OtherJobSettings {
	out := make([]OtherJobSettings, len(defaultOtherJobs))
	copy(out, defaultOtherJobs)
	return out
}

// ParseClock parses a 24-hour local time "HH:MM".
func ParseClock(value string) (hour, minute int, error error) {
	parts := strings.Split(value, ":")
	if len(parts) != scheduleTimePatternParts {
		return 0, 0, fmt.Errorf("job: invalid time %q: want HH:MM", value)
	}
	h, err := parseTwoDigit(parts[0], 23)
	if err != nil {
		return 0, 0, fmt.Errorf("job: invalid time %q: %w", value, err)
	}
	m, err := parseTwoDigit(parts[1], 59)
	if err != nil {
		return 0, 0, fmt.Errorf("job: invalid time %q: %w", value, err)
	}
	return h, m, nil
}

func parseTwoDigit(s string, max int) (int, error) {
	if len(s) != 2 {
		return 0, fmt.Errorf("want two digits, got %q", s)
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit in %q", s)
		}
		n = n*10 + int(c-'0')
	}
	if n > max {
		return 0, fmt.Errorf("%d exceeds %d", n, max)
	}
	return n, nil
}

// ChainSchedulePreview returns a human-readable chain schedule summary.
func ChainSchedulePreview(startTime string) string {
	return fmt.Sprintf("every day at %s", startTime)
}

// OtherJobSchedulePreview returns a human-readable separately scheduled job summary.
func OtherJobSchedulePreview(freq Frequency, time string) string {
	switch freq {
	case FrequencyDaily:
		return fmt.Sprintf("every day at %s", time)
	case FrequencyWeekly:
		return fmt.Sprintf("every Sunday at %s", time)
	case FrequencyMonthly:
		return fmt.Sprintf("on the 1st of each month at %s", time)
	default:
		return fmt.Sprintf("at %s", time)
	}
}

// NextChainRun returns the next local instant the maintenance chain starts.
func NextChainRun(now time.Time, loc *time.Location, startTime string) time.Time {
	hour, minute, err := ParseClock(startTime)
	if err != nil {
		hour, minute, _ = ParseClock(DefaultChainStartTime)
	}
	year, month, day := now.In(loc).Date()
	candidate := time.Date(year, month, day, hour, minute, 0, 0, loc)
	if !candidate.After(now.In(loc)) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate.UTC()
}

// NextOtherJobRun returns the next local instant a separately scheduled job runs.
// Weekly jobs use Sunday (DefaultWeeklyScrubDay) as the implicit weekday;
// monthly jobs use the 1st. Doc 03 §8.4's UI is frequency + time only.
func NextOtherJobRun(now time.Time, loc *time.Location, job OtherJobSettings) time.Time {
	hour, minute, err := ParseClock(job.Time)
	if err != nil {
		hour, minute, _ = ParseClock("00:00")
	}
	localNow := now.In(loc)
	year, month, day := localNow.Date()
	candidate := time.Date(year, month, day, hour, minute, 0, 0, loc)

	switch job.Frequency {
	case FrequencyWeekly:
		days := (int(time.Sunday) - int(candidate.Weekday()) + 7) % 7
		candidate = candidate.AddDate(0, 0, days)
		if !candidate.After(localNow) {
			candidate = candidate.AddDate(0, 0, 7)
		}
		return candidate.UTC()
	case FrequencyMonthly:
		candidate = time.Date(year, month, 1, hour, minute, 0, 0, loc)
		if !candidate.After(localNow) {
			candidate = candidate.AddDate(0, 1, 0)
		}
		return candidate.UTC()
	default:
		if candidate.After(localNow) {
			return candidate.UTC()
		}
		return candidate.AddDate(0, 0, 1).UTC()
	}
}

// ChainWindows builds ScheduledWindows for the maintenance chain's
// job-backed steps, starting at start and honouring weeklyScrubDay for scrub.
// loc is the installation timezone; start may be UTC from NextChainRun.
func ChainWindows(start time.Time, loc *time.Location, weeklyScrubDay int, enabled map[Step]bool) []NamedWindow {
	if enabled == nil {
		enabled = defaultChainEnabled
	}
	if loc == nil {
		loc = time.UTC
	}
	cursor := start
	weekly := start.In(loc).Weekday() == time.Weekday(weeklyScrubDay)
	var out []NamedWindow
	for _, step := range chainOrder {
		if step == StepScrub && !weekly {
			continue
		}
		if on, ok := enabled[step]; ok && !on {
			continue
		}
		class, ok := stepClass(step)
		if !ok {
			cursor = cursor.Add(stepDurationEstimates[step])
			continue
		}
		dur := stepDurationEstimates[step]
		out = append(out, NamedWindow{
			JobID: MaintenanceChainJobID,
			Window: ScheduledWindow{
				Class:    class,
				Start:    cursor,
				Duration: dur,
			},
		})
		cursor = cursor.Add(dur)
	}
	return out
}

func stepClass(step Step) (Class, bool) {
	switch step {
	case StepMover:
		return ClassArrayWrite, true
	case StepSync, StepScrub:
		return ClassParity, true
	default:
		return "", false
	}
}

// OtherJobWindow builds the next ScheduledWindow for a separately scheduled job.
func OtherJobWindow(start time.Time, job OtherJobSettings) NamedWindow {
	class := otherJobClasses[job.ID]
	dur := otherJobDurationEstimates[job.ID]
	if dur == 0 {
		dur = time.Hour
	}
	return NamedWindow{
		JobID: job.ID,
		Window: ScheduledWindow{
			Class:    class,
			Start:    start,
			Duration: dur,
		},
	}
}

// DetectScheduleConflicts finds every pair of named windows that would
// collide per DetectConflict (doc 01 §4).
func DetectScheduleConflicts(windows []NamedWindow) []ScheduleConflict {
	var conflicts []ScheduleConflict
	for i := 0; i < len(windows); i++ {
		for j := i + 1; j < len(windows); j++ {
			a, b := windows[i], windows[j]
			if !DetectConflict(a.Window, b.Window) {
				continue
			}
			jobA, jobB := a.JobID, b.JobID
			if jobA > jobB {
				jobA, jobB = jobB, jobA
			}
			dup := false
			for _, c := range conflicts {
				if c.JobA == jobA && c.JobB == jobB {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			conflicts = append(conflicts, ScheduleConflict{JobA: jobA, JobB: jobB})
		}
	}
	return conflicts
}

// BuildScheduleWindows collects every named window for conflict detection.
func BuildScheduleWindows(now time.Time, loc *time.Location, chain ChainSettings, others []OtherJobSettings) []NamedWindow {
	chainStart := NextChainRun(now, loc, chain.StartTime)
	windows := ChainWindows(chainStart, loc, chain.WeeklyScrubDay, chain.Enabled)
	for _, job := range others {
		if !job.Enabled {
			continue
		}
		start := NextOtherJobRun(now, loc, job)
		windows = append(windows, OtherJobWindow(start, job))
	}
	return windows
}
