package main

import (
	"context"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/web/fixtures"
)

func (h *handler) GetParity(ctx context.Context) (*apiv1.ParitySnapshot, error) {
	return mockParitySnapshot(h.scenario)
}

func (h *handler) RunParityDiff(ctx context.Context) (*apiv1.ParityDiffResult, error) {
	snap, err := mockParitySnapshot(h.scenario)
	if err != nil {
		return nil, err
	}
	guard, ok := snap.Guard.Get()
	if !ok {
		guard = apiv1.ParityGuardState{WouldBlock: false}
	}
	return &apiv1.ParityDiffResult{
		Groups: snap.Groups,
		Guard:  guard,
	}, nil
}

func mockParitySnapshot(scenario string) (*apiv1.ParitySnapshot, error) {
	raw, err := fixtures.ParityJSON(scenario)
	if err != nil {
		return fallbackParitySnapshot(scenario)
	}
	var snap apiv1.ParitySnapshot
	if err := snap.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("decode parity fixture: %w", err)
	}
	if err := snap.Validate(); err != nil {
		return nil, fmt.Errorf("parity fixture: %w", err)
	}
	return &snap, nil
}

func fallbackParitySnapshot(scenario string) (*apiv1.ParitySnapshot, error) {
	freshness := apiv1.ParityFreshnessGreen
	if scenario == "fresh-install" {
		freshness = apiv1.ParityFreshnessAmber
	}
	return &apiv1.ParitySnapshot{
		Freshness:        freshness,
		ChangedSinceSync: apiv1.NewOptInt32(0),
		DataDisks:        apiv1.NewOptInt32(0),
		ParityDisks:      apiv1.NewOptInt32(0),
		Guard:            apiv1.NewOptParityGuardState(apiv1.ParityGuardState{WouldBlock: false}),
	}, nil
}
