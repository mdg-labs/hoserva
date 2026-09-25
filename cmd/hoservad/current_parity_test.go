package main

import (
	"context"
	"errors"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/parity"
)

// TestCurrentParityEngine_ResolvesAtCallTime covers a daemon started with
// no array: the disk replace and upgrade jobs are wired before any parity
// engine exists, and must reach the one a live array creation wires
// later (#265) rather than a nil snapshot taken at startup.
func TestCurrentParityEngine_ResolvesAtCallTime(t *testing.T) {
	ctx := context.Background()
	handler := &api.Handler{}
	eng := currentParityEngine{handler: handler}

	if _, err := eng.Status(ctx); !errors.Is(err, errNoParityEngine) {
		t.Fatalf("Status with no engine = %v, want errNoParityEngine", err)
	}
	var typedNil *parity.SnapraidEngine
	handler.SetParity(typedNil, parity.Guard{}, nil, nil)
	if _, err := eng.Diff(ctx); !errors.Is(err, errNoParityEngine) {
		t.Fatalf("Diff with a typed-nil engine = %v, want errNoParityEngine", err)
	}

	fake := parity.NewFakeEngine()
	fake.SetDiff(parity.DiffReport{Added: 3})
	handler.SetParity(fake, parity.Guard{}, nil, nil)
	report, err := eng.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after live wiring: %v", err)
	}
	if report.Added != 3 {
		t.Fatalf("Diff = %+v, want the live engine's report", report)
	}
}
