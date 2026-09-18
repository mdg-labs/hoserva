package parity

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrStatusParse wraps a `snapraid status -l <file>` log ParseStatus could
// not make sense of.
var ErrStatusParse = errors.New("parity: could not parse snapraid status output")

// StatusReport is everything a `snapraid status -l <log>` run states,
// parsed from its structured log rather than its human-formatted table
// (doc 02 §2). It carries more than the public ParityStatus: Engine.Diff
// also runs status first (diff's own log never states how many files a
// disk had *before* the changes it reports) and uses PerDiskFileCount and
// DataMounts as BuildDiffReport's "before" snapshot.
type StatusReport struct {
	// DataMounts maps SnapRAID's own disk id ("d1") to the mount path the
	// config assigned it, read from the log's own config echo.
	DataMounts map[string]string
	// PerDiskFileCount is `summary:disk_file_count:<id>:<n>` per disk.
	PerDiskFileCount map[string]int
	// ZeroSubsecondFiles is `summary:zerosubsecond_file_count` — Q17's own
	// trigger: run `touch` before a sync only when this is > 0.
	ZeroSubsecondFiles int
	// ChangedSinceSync is `summary:has_unsynced`.
	ChangedSinceSync int
	// Unscrubbed is `summary:has_unscrubbed`.
	Unscrubbed int
	// BadBlocks is `summary:has_bad`'s own first field — blocks scrub
	// marked bad that a fix has not yet cleared (doc 02 §2's scrub→fix
	// cycle).
	BadBlocks int
	// ParityDisks is 1, or 2 once a `2-parity:` line is present (Q19).
	ParityDisks int
	// LastActivityAt is the newest `info_time` timestamp the log
	// carries — the closest real signal a `status` run alone has for
	// "when did the array last do anything" (sync or scrub both record
	// one); it is not specifically "last sync time" on its own.
	LastActivityAt time.Time
}

// ParseStatus parses a `snapraid status -l <log>` run's structured log
// (doc 02 §2, Q17).
func ParseStatus(log []byte) (StatusReport, error) {
	lines := splitLogLines(log)
	if lines == nil {
		return StatusReport{}, fmt.Errorf("%w: empty log", ErrStatusParse)
	}

	report := StatusReport{
		DataMounts:       map[string]string{},
		PerDiskFileCount: map[string]int{},
	}
	sawSummary := false
	dualParity := false

	for _, line := range lines {
		tag, rest, ok := cutTag(line)
		if !ok {
			continue
		}
		switch tag {
		case "data":
			id, path, ok := strings.Cut(rest, ":")
			if ok {
				report.DataMounts[id] = path
			}
		case "2-parity":
			dualParity = true
		case "info_time":
			fields := strings.Split(rest, ":")
			if len(fields) == 0 {
				continue
			}
			unix, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil {
				continue
			}
			t := time.Unix(unix, 0)
			if t.After(report.LastActivityAt) {
				report.LastActivityAt = t
			}
		case "summary":
			sawSummary = true
			parseStatusSummaryField(&report, rest)
		}
	}

	if !sawSummary {
		return StatusReport{}, fmt.Errorf("%w: no summary section found", ErrStatusParse)
	}

	report.ParityDisks = 1
	if dualParity {
		report.ParityDisks = 2
	}
	return report, nil
}

func parseStatusSummaryField(report *StatusReport, rest string) {
	fields := strings.Split(rest, ":")
	if len(fields) < 2 {
		return
	}
	switch fields[0] {
	case "disk_file_count":
		if len(fields) == 3 {
			if n, err := strconv.Atoi(fields[2]); err == nil {
				report.PerDiskFileCount[fields[1]] = n
			}
		}
	case "zerosubsecond_file_count":
		if n, err := strconv.Atoi(fields[1]); err == nil {
			report.ZeroSubsecondFiles = n
		}
	case "has_unsynced":
		if n, err := strconv.Atoi(fields[1]); err == nil {
			report.ChangedSinceSync = n
		}
	case "has_unscrubbed":
		if n, err := strconv.Atoi(fields[1]); err == nil {
			report.Unscrubbed = n
		}
	case "has_bad":
		if n, err := strconv.Atoi(fields[1]); err == nil {
			report.BadBlocks = n
		}
	}
}

// ToParityStatus distills r to the public ParityStatus (doc 02 §2's
// dashboard indicator). Freshness only reflects what a single `status`
// run can see for itself: pending errors (Red) or unsynced changes
// (Amber) — the "parity older than 3x the schedule interval" Red case
// needs the configured schedule, which this package does not carry, so
// that comparison is left to Freshness's own caller.
func (r StatusReport) ToParityStatus() ParityStatus {
	freshness := FreshnessGreen
	switch {
	case r.BadBlocks > 0:
		freshness = FreshnessRed
	case r.ChangedSinceSync > 0:
		freshness = FreshnessAmber
	}
	return ParityStatus{
		Freshness:        freshness,
		LastSyncAt:       r.LastActivityAt,
		ChangedSinceSync: r.ChangedSinceSync,
		DataDisks:        len(r.DataMounts),
		ParityDisks:      r.ParityDisks,
	}
}
