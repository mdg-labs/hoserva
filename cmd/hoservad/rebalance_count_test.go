package main

import (
	"context"
	"testing"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// The rebalance and evacuation limit divides by the same total as the
// guard's removed-percent rule: regular files only. Counting the links
// PerDisk[...].FilesBefore includes would enlarge the denominator and let
// them move more than the guard would allow.
func TestRebalanceTrackedFileCount_ExcludesLinks(t *testing.T) {
	eng := parity.NewFakeEngine()
	eng.SetDiff(parity.DiffReport{PerDisk: map[string]parity.DiskDiff{
		"/m/d1": {FilesBefore: 1000, FilesAfter: 1000, LinksBefore: 900},
		"/m/d2": {FilesBefore: 50, FilesAfter: 50},
	}})

	got, err := rebalanceTrackedFileCount(eng)(context.Background())
	if err != nil {
		t.Fatalf("rebalanceTrackedFileCount: %v", err)
	}
	if got != 150 {
		t.Fatalf("count = %d, want 150 regular files (100 on d1, 50 on d2), not the 1050 entries with links", got)
	}
}
