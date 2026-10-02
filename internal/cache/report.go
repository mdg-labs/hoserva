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
	// ResultSkippedNotRegular is an entry of a type this package has no
	// way to recreate. Symlinks, FIFOs and device nodes are recreated and
	// sockets are ResultSkippedSocket, so nothing a Linux filesystem
	// holds reaches it.
	ResultSkippedNotRegular Result = "skipped_not_regular"
	// ResultSkippedSocket is a Unix socket. A socket is runtime state its
	// owner recreates when it starts, so it is reported and left where it
	// is rather than copied.
	ResultSkippedSocket Result = "skipped_socket"
	// ResultLeftBehind is an entry still on the source when the relocation
	// ended that no entry of this run explains: an earlier run of the same
	// job left it, and its reason was not carried across the resume.
	ResultLeftBehind Result = "left_behind"
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
	// Kind names the entry's type when it is not a regular file:
	// "symlink", "fifo", "char_device", "block_device" or "socket".
	Kind   string
	Reason string
	Err    string
}

func (e Entry) reasonSuffix() string {
	kind := ""
	if e.Kind != "" {
		kind = " (" + e.Kind + ")"
	}
	switch {
	case e.Err != "":
		return kind + ": " + e.Err
	case e.Reason != "":
		return kind + ": " + e.Reason
	default:
		return kind
	}
}

// Report is one mover run's outcome across every share it processed
// (doc 09 §2's run report acceptance criterion).
type Report struct {
	StartedAt   time.Time
	FinishedAt  time.Time
	Interrupted bool
	// Relocation is set on the report of a share relocation, where every
	// entry the run did not move is something the user asked to have
	// moved. A scheduled mover pass leaves it false: files inside the
	// grace period or held open are routine there.
	Relocation bool
	Entries    []Entry
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
			ResultSkippedNoSpace, ResultSkippedNotRegular, ResultSkippedSocket, ResultSkippedGone, ResultConflict:
			out = append(out, e)
		}
	}
	return out
}

// LeftBehind returns every entry whose source is still where it was after
// the run, for a reason that is not "it no longer exists": skipped, in
// conflict, failed, or copied but not yet deleted. Sockets are included;
// RuntimeOnly separates them out.
func (r Report) LeftBehind() []Entry {
	var out []Entry
	for _, e := range r.Entries {
		switch e.Result {
		case ResultMoved, ResultSkippedGone:
		default:
			out = append(out, e)
		}
	}
	return out
}

// Incomplete reports whether a relocation left anything but runtime-only
// sockets behind, so the share's files are not all on the new side yet.
func (r Report) Incomplete() bool {
	if !r.Relocation {
		return false
	}
	for _, e := range r.LeftBehind() {
		if e.Result != ResultSkippedSocket {
			return true
		}
	}
	return false
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
	summary := fmt.Sprintf(
		"mover: %d moved (%d bytes), %d skipped, %d pending delete, %d failed, duration %s",
		counts[ResultMoved], r.MovedBytes(),
		len(r.Skipped()),
		counts[ResultMovedPendingDelete],
		counts[ResultFailed],
		r.FinishedAt.Sub(r.StartedAt),
	)
	if !r.Relocation {
		return summary
	}
	if sockets := counts[ResultSkippedSocket]; sockets > 0 {
		summary += fmt.Sprintf("; %d runtime-only socket(s) left in place", sockets)
	}
	if r.Incomplete() {
		left := len(r.LeftBehind()) - counts[ResultSkippedSocket]
		summary += fmt.Sprintf("; relocation incomplete: %d left behind (see the entries above for each path and why)", left)
	}
	return summary
}
