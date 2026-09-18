package parity

import (
	"bufio"
	"bytes"
	"testing"
)

// TestParseProgressLine_RealCapture replays a real sync's raw stdout
// (snapraid_sync_stdout_progress.log, captured with `\r` intact — no
// `-l`, the plain human-formatted progress SnapRAID prints while
// running), split the same way CommandRunner splits it, and checks that
// exactly the two progress lines it contains parse, with every other
// line correctly not matching.
func TestParseProgressLine_RealCapture(t *testing.T) {
	data := readCorpus(t, "snapraid_sync_stdout_progress.log")
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Split(scanLinesOrCR)

	var ticks []Progress
	for scanner.Scan() {
		if p, ok := parseProgressLine(scanner.Text()); ok {
			ticks = append(ticks, p)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning fixture: %v", err)
	}

	if len(ticks) != 2 {
		t.Fatalf("got %d progress ticks, want 2 (a live '%%, MB' tick and a 'completed' tick): %+v", len(ticks), ticks)
	}
	if ticks[0].Percent != 0 {
		t.Errorf("ticks[0].Percent = %v, want 0", ticks[0].Percent)
	}
	if ticks[1].Percent != 100 || ticks[1].BytesDone != 32*1024*1024 {
		t.Errorf("ticks[1] = %+v, want Percent=100 BytesDone=32MiB", ticks[1])
	}
}

func TestParseProgressLine_NonProgressLines(t *testing.T) {
	for _, line := range []string{
		"Syncing...",
		"Everything OK",
		"     d1 23% | **************",
		"WARNING! UUID is unsupported for disks: 'd1', 'd2', 'd3'.",
	} {
		if _, ok := parseProgressLine(line); ok {
			t.Errorf("parseProgressLine(%q): matched as a progress tick, want no match", line)
		}
	}
}
