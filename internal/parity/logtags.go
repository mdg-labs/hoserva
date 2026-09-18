package parity

import (
	"strconv"
	"strings"
)

// splitLogLines splits one `snapraid -l <file>` run's structured,
// machine-readable log (SnapRAID's own format, confirmed against a real
// snapraid 12.4-1 binary in the loop-device lab — testdata/parsers'
// snapraid_*.log fixtures, and spikes/s5/results/fix-d2.log) into its
// individual "tag:field:field:..." lines, dropping the trailing newline
// SnapRAID always ends the file with. Every parser in this package works
// from this slice, since every -l log shares this one line shape.
func splitLogLines(data []byte) []string {
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// cutTag splits one log line into its leading tag ("summary", "scan",
// "data", "status", ...) and everything after the first ":". Lines this
// package has no use for (msg:, memory:, uuid:, statfs:, ...) still parse
// fine here; callers just never match their tag in a switch.
func cutTag(line string) (tag, rest string, ok bool) {
	return strings.Cut(line, ":")
}

// atoiField parses fields[idx] as an int, the shared last step behind
// every "key:n" (diff_parse.go, run_parse.go) and "key:sub:n"
// (status_parse.go's disk_file_count) summary field this package's three
// log parsers each read. ok is false, and the field left untouched by the
// caller, when idx is out of range or the token isn't a valid integer —
// a summary field snapraid itself never documents is simply skipped, not
// a parse error (every ParseXSummary caller already tolerates unknown
// tags the same way).
func atoiField(fields []string, idx int) (n int, ok bool) {
	if idx >= len(fields) {
		return 0, false
	}
	n, err := strconv.Atoi(fields[idx])
	if err != nil {
		return 0, false
	}
	return n, true
}
