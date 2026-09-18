package parity

import "strconv"

// DefaultScrubOlderThanDays is doc 02 §2's own scheduled-scrub default:
// 8% of the array, older than 10 days.
const DefaultScrubOlderThanDays = 10

// Every function here returns only the operation-specific tail of a
// snapraid invocation's argv — SnapraidEngine prepends "-c <conf> -l
// <log>", shared by every operation, once. Building argv as a slice
// rather than a string, and execing it directly, is what keeps every
// invocation an argv, never a shell command (CLAUDE.md).

func diffArgv() []string   { return []string{"diff"} }
func statusArgv() []string { return []string{"status"} }
func touchArgv() []string  { return []string{"touch"} }

// syncArgv builds sync's own tail. force maps SyncOpts.Force to `-E`
// (`--force-empty`): SnapRAID refuses a sync that would empty a
// previously non-empty disk on its own, independently of Hoserva's own
// threshold guard (doc 02 §2) — confirmed against a real sync in the
// loop-device lab ("WARNING! ... are now missing or have been rewritten!
// ... use 'snapraid --force-empty sync'."). By the time Engine.Sync is
// called with Force set, the guard above it has already made that
// decision; this only carries it through to the one flag SnapRAID itself
// needs to not refuse redundantly.
func syncArgv(force bool) []string {
	var argv []string
	if force {
		argv = append(argv, "-E")
	}
	return append(argv, "sync")
}

// scrubArgv builds scrub's own tail: `-p <pct> -o <olderThanDays>`
// (doc 02 §2).
func scrubArgv(pct, olderThanDays int) []string {
	return []string{"-p", strconv.Itoa(pct), "-o", strconv.Itoa(olderThanDays), "scrub"}
}

func fixArgv(opts FixOpts) []string {
	var argv []string
	if opts.Disk != "" {
		argv = append(argv, "-d", opts.Disk)
	}
	if opts.Path != "" {
		argv = append(argv, "-f", opts.Path)
	}
	if opts.ErrorsOnly {
		argv = append(argv, "-e")
	}
	return append(argv, "fix")
}

func checkArgv(opts CheckOpts) []string {
	var argv []string
	if opts.Disk != "" {
		argv = append(argv, "-d", opts.Disk)
	}
	if opts.AuditOnly {
		argv = append(argv, "-a")
	}
	return append(argv, "check")
}
