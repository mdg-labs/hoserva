package cache

import (
	"fmt"
	"time"
)

// Result is one file's outcome in a mover Report (doc 09 §2's "Honest
// reporting": "files moved, bytes, duration, files skipped and why").
type Result string

const (
	// ResultMoved is a file fully relocated to the array in this run.
	ResultMoved Result = "moved"
	// ResultMovedPendingDelete is a file copied and renamed into place on
	// the array, but whose cache-side source is left behind because it
	// became open between the copy and the final re-check (doc 09 §2's
	// re-check-before-unlink step). It is safe — both copies are
	// complete and correct, the source is simply not yet reclaimed — and
	// a future run's pending-relocation check completes the delete.
	ResultMovedPendingDelete Result = "moved_pending_delete"
	// ResultSkippedOpen is a file held open by some process, including
	// mergerfs itself on behalf of a client using the union path.
	ResultSkippedOpen Result = "skipped_open"
	// ResultSkippedGrace is a file modified more recently than the grace
	// period allows.
	ResultSkippedGrace Result = "skipped_grace_period"
	// ResultSkippedExcluded is a file matching one of the share's
	// exclude patterns.
	ResultSkippedExcluded Result = "skipped_excluded"
	// ResultSkippedNoSpace is a file the array has no eligible disk with
	// room for (size plus minfreespace).
	ResultSkippedNoSpace Result = "skipped_no_space"
	// ResultSkippedNotRegular is a non-regular file (directory, symlink,
	// device, socket) the mover leaves alone.
	ResultSkippedNotRegular Result = "skipped_not_regular"
	// ResultSkippedGone is a file that disappeared between enumeration
	// and processing — not an error, just no longer there to move.
	ResultSkippedGone Result = "skipped_gone"
	// ResultConflict is a file whose array-side path already exists with
	// a different size than the cache-side source — never auto-resolved,
	// since guessing which copy is authoritative could lose data.
	ResultConflict Result = "conflict"
	// ResultFailed is a file the mover could not process; Err carries
	// the reason.
	ResultFailed Result = "failed"
)

// Entry is one file's outcome, with the reason a skip or failure
// happened.
type Entry struct {
	Share  string
	Path   string
	Bytes  int64
	Result Result
	Reason string
	Err    string
}

func (e Entry) reasonSuffix() string {
	switch {
	case e.Err != "":
		return ": " + e.Err
	case e.Reason != "":
		return ": " + e.Reason
	default:
		return ""
	}
}

// Report is one mover run's outcome across every share it processed
// (doc 09 §2's run report acceptance criterion).
type Report struct {
	StartedAt   time.Time
	FinishedAt  time.Time
	Interrupted bool
	Entries     []Entry
}

func (r *Report) add(e Entry) { r.Entries = append(r.Entries, e) }

// Moved returns every entry that fully completed a relocation in this
// run (ResultMoved only — a pending-delete entry has not finished).
func (r Report) Moved() []Entry { return r.byResult(ResultMoved) }

// Skipped returns every entry the mover chose not to move, with why.
func (r Report) Skipped() []Entry {
	var out []Entry
	for _, e := range r.Entries {
		switch e.Result {
		case ResultSkippedOpen, ResultSkippedGrace, ResultSkippedExcluded,
			ResultSkippedNoSpace, ResultSkippedNotRegular, ResultSkippedGone, ResultConflict:
			out = append(out, e)
		}
	}
	return out
}

// Failed returns every entry the mover could not process.
func (r Report) Failed() []Entry { return r.byResult(ResultFailed) }

func (r Report) byResult(want Result) []Entry {
	var out []Entry
	for _, e := range r.Entries {
		if e.Result == want {
			out = append(out, e)
		}
	}
	return out
}

// MovedBytes totals the bytes of every fully completed move.
func (r Report) MovedBytes() int64 {
	var total int64
	for _, e := range r.Entries {
		if e.Result == ResultMoved {
			total += e.Bytes
		}
	}
	return total
}

// Summary renders a one-line human summary for the job log (doc 09 §2's
// "honest reporting" — never silent about what was skipped and why).
func (r Report) Summary() string {
	counts := make(map[Result]int, len(r.Entries))
	for _, e := range r.Entries {
		counts[e.Result]++
	}
	return fmt.Sprintf(
		"mover: %d moved (%d bytes), %d skipped, %d pending delete, %d failed, duration %s",
		counts[ResultMoved], r.MovedBytes(),
		len(r.Skipped()),
		counts[ResultMovedPendingDelete],
		counts[ResultFailed],
		r.FinishedAt.Sub(r.StartedAt),
	)
}
