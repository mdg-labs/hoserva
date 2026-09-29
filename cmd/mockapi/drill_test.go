package main

import (
	"context"
	"testing"
	"time"
)

func TestMockRestoreDrill_ARunReplacesTheSeededResult(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	before, err := h.GetRestoreDrill(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seeded, ok := before.LastRun.Get()
	if !ok || !seeded.Passed || len(seeded.Destinations) != 2 {
		t.Fatalf("seeded last run = %+v, %v; want a pass over the two default destinations", seeded, ok)
	}

	if _, err := h.StartRestoreDrill(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := h.GetRestoreDrill(ctx)
	run, _ := after.LastRun.Get()
	if !run.RanAt.After(seeded.RanAt) || time.Since(run.RanAt) > time.Minute {
		t.Fatalf("last run after a drill = %v, want the time of the run just started (was %v)", run.RanAt, seeded.RanAt)
	}
}
