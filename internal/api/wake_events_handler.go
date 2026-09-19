package api

import (
	"context"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/store"
)

func (h *Handler) ListWakeEvents(ctx context.Context) (*apiv1.WakeEventsResponse, error) {
	if h.History == nil {
		return &apiv1.WakeEventsResponse{
			Events:          []apiv1.SpinTransition{},
			DailyWakeCounts: []apiv1.DailyWakeCount{},
		}, nil
	}

	events, daily, err := h.History.ListWakeEvents(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing wake events: %w", err)
	}
	return &apiv1.WakeEventsResponse{
		Events:          spinTransitionsToAPI(events),
		DailyWakeCounts: dailyWakeCountsToAPI(daily),
	}, nil
}

func spinTransitionsToAPI(events []store.ListedSpinEvent) []apiv1.SpinTransition {
	out := make([]apiv1.SpinTransition, 0, len(events))
	for _, ev := range events {
		row := apiv1.SpinTransition{
			Device:    ev.Device,
			FromState: apiv1.SpinTransitionFromState(ev.FromState),
			ToState:   apiv1.SpinTransitionToState(ev.ToState),
			At:        ev.At,
		}
		if ev.AwakeDurationSeconds != nil {
			row.AwakeDurationSeconds = apiv1.NewOptNilInt64(*ev.AwakeDurationSeconds)
		}
		out = append(out, row)
	}
	return out
}

func dailyWakeCountsToAPI(daily []store.ListedDailyWakeCount) []apiv1.DailyWakeCount {
	out := make([]apiv1.DailyWakeCount, 0, len(daily))
	for _, row := range daily {
		out = append(out, apiv1.DailyWakeCount{
			Device: row.Device,
			Date:   row.Date,
			Count:  row.Count,
		})
	}
	return out
}
