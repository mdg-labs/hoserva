package parity

import (
	"regexp"
	"strconv"
)

// SnapRAID rewrites its own progress line in place with a carriage return
// rather than emitting a newline per update (confirmed against a real
// sync and scrub in the loop-device lab and in spike S5,
// spikes/s5/results/run-all.log): a run's raw output contains
// "12%, 34 MB          \r56%, 78 MB          \r100% completed, 90 MB
// accessed in 0:01    \n". CommandRunner's line splitter treats \r as a
// line terminator too, so each of those arrives here as its own line.
var (
	progressTickRe = regexp.MustCompile(`^(\d+)%,\s*(\d+)\s*MB\b`)
	progressDoneRe = regexp.MustCompile(`^(\d+)%\s*completed,\s*(\d+)\s*MB accessed`)
)

// parseProgressLine reports whether line is one of SnapRAID's own
// progress updates, and the Progress it represents if so. A line that
// doesn't match (a warning, a phase name like "Syncing...", a table row)
// is not a progress update; the caller still gets to see it via
// Progress.Output for the job's captured log, just without touching
// Percent/BytesDone.
func parseProgressLine(line string) (Progress, bool) {
	if m := progressDoneRe.FindStringSubmatch(line); m != nil {
		return Progress{Percent: mustAtof(m[1]), BytesDone: mustMB(m[2]), Output: line}, true
	}
	if m := progressTickRe.FindStringSubmatch(line); m != nil {
		return Progress{Percent: mustAtof(m[1]), BytesDone: mustMB(m[2]), Output: line}, true
	}
	return Progress{}, false
}

func mustAtof(s string) float64 {
	n, _ := strconv.Atoi(s)
	return float64(n)
}

func mustMB(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n * 1024 * 1024
}
