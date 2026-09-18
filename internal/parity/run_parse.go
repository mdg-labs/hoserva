package parity

import (
	"errors"
	"fmt"
	"strings"
)

// ErrRunParse wraps a sync/scrub/fix/check `-l <file>` log ParseRunSummary
// could not make sense of.
var ErrRunParse = errors.New("parity: could not parse snapraid run output")

// RunSummary is the result sync, scrub, fix and check (doc 01 §4's Parity
// job class) each report in their own log's `summary:` lines once they
// finish. Which fields apply, and whether finding a non-zero one means
// "the job failed" rather than "the job completed and found something" —
// scrub's whole purpose is finding data errors, fix's is reporting what it
// could and couldn't recover — differs per operation; SnapraidEngine's own
// callers decide that. RunSummary only carries what the log said.
type RunSummary struct {
	// Exit is `summary:exit`'s own value: "ok", "diff", "error",
	// "recovered", confirmed against a real snapraid 12.4-1 binary.
	Exit string
	// FileErrors, IOErrors and DataErrors are sync/scrub's own
	// `summary:error_file`/`_io`/`_data`.
	FileErrors, IOErrors, DataErrors int
	// Errors, Recovered and Unrecoverable are fix/check's own
	// `summary:error`/`_recovered`/`_unrecoverable`.
	Errors, Recovered, Unrecoverable int
	// RecoveredFiles is fix's own `status:recovered:<disk>:<path>` lines,
	// one per file fix actually rewrote from parity.
	RecoveredFiles []string
}

// ParseRunSummary parses a sync, scrub, fix or check run's structured
// `-l <log>` (doc 02 §2).
func ParseRunSummary(log []byte) (RunSummary, error) {
	lines := splitLogLines(log)
	if lines == nil {
		return RunSummary{}, fmt.Errorf("%w: empty log", ErrRunParse)
	}

	var s RunSummary
	sawSummary := false

	for _, line := range lines {
		tag, rest, ok := cutTag(line)
		if !ok {
			continue
		}
		switch tag {
		case "summary":
			sawSummary = true
			parseRunSummaryField(&s, rest)
		case "status":
			kind, tail, ok := strings.Cut(rest, ":")
			if ok && kind == "recovered" {
				s.RecoveredFiles = append(s.RecoveredFiles, tail)
			}
		}
	}

	if !sawSummary {
		return RunSummary{}, fmt.Errorf("%w: no summary section found", ErrRunParse)
	}
	return s, nil
}

func parseRunSummaryField(s *RunSummary, rest string) {
	fields := strings.Split(rest, ":")
	if len(fields) < 2 {
		return
	}
	if fields[0] == "exit" {
		s.Exit = fields[1]
		return
	}
	n, ok := atoiField(fields, 1)
	if !ok {
		return
	}
	switch fields[0] {
	case "error_file":
		s.FileErrors = n
	case "error_io":
		s.IOErrors = n
	case "error_data":
		s.DataErrors = n
	case "error":
		s.Errors = n
	case "error_recovered":
		s.Recovered = n
	case "error_unrecoverable":
		s.Unrecoverable = n
	}
}
