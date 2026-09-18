package parity

import "strings"

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
