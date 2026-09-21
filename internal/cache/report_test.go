package cache

import (
	"strings"
	"testing"
	"time"
)

func TestReport_MovedSkippedFailed(t *testing.T) {
	r := Report{
		StartedAt:  time.Unix(0, 0),
		FinishedAt: time.Unix(10, 0),
		Entries: []Entry{
			{Path: "a", Result: ResultMoved, Bytes: 100},
			{Path: "b", Result: ResultSkippedOpen},
			{Path: "c", Result: ResultSkippedGrace},
			{Path: "d", Result: ResultFailed, Err: "boom"},
			{Path: "e", Result: ResultMovedPendingDelete, Bytes: 5},
		},
	}
	if got := len(r.Moved()); got != 1 {
		t.Errorf("Moved() = %d, want 1", got)
	}
	if got := len(r.Skipped()); got != 2 {
		t.Errorf("Skipped() = %d, want 2", got)
	}
	if got := len(r.Failed()); got != 1 {
		t.Errorf("Failed() = %d, want 1", got)
	}
	if got := r.MovedBytes(); got != 100 {
		t.Errorf("MovedBytes() = %d, want 100", got)
	}
	summary := r.Summary()
	for _, want := range []string{"1 moved", "2 skipped", "1 pending delete", "1 failed"} {
		if !strings.Contains(summary, want) {
			t.Errorf("Summary() = %q, want it to contain %q", summary, want)
		}
	}
}

func TestEntry_ReasonSuffix(t *testing.T) {
	cases := []struct {
		e    Entry
		want string
	}{
		{Entry{}, ""},
		{Entry{Reason: "why"}, ": why"},
		{Entry{Err: "boom"}, ": boom"},
		{Entry{Reason: "why", Err: "boom"}, ": boom"},
	}
	for _, tc := range cases {
		if got := tc.e.reasonSuffix(); got != tc.want {
			t.Errorf("reasonSuffix() = %q, want %q", got, tc.want)
		}
	}
}
