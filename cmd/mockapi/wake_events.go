package main

import (
	"context"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func mockWakeEvents(scenario string) *apiv1.WakeEventsResponse {
	if scenario == "fresh-install" {
		return &apiv1.WakeEventsResponse{
			Events:          []apiv1.SpinTransition{},
			DailyWakeCounts: []apiv1.DailyWakeCount{},
		}
	}

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	wake1 := today.Add(2 * time.Hour)
	standby1 := wake1.Add(45 * time.Minute)
	wake2 := today.Add(6 * time.Hour)
	standby2 := wake2.Add(12 * time.Minute)
	yesterday := today.Add(-24 * time.Hour)
	wakeYesterday := yesterday.Add(20 * time.Hour)

	events := []apiv1.SpinTransition{
		{
			Device:    "/dev/sdb",
			FromState: apiv1.SpinTransitionFromStateActive,
			ToState:   apiv1.SpinTransitionToStateStandby,
			At:        standby2,
		},
		{
			Device:               "/dev/sdb",
			FromState:            apiv1.SpinTransitionFromStateStandby,
			ToState:              apiv1.SpinTransitionToStateActive,
			At:                   wake2,
			AwakeDurationSeconds: apiv1.NewOptNilInt64(int64(standby2.Sub(wake2).Seconds())),
		},
		{
			Device:    "/dev/sdb",
			FromState: apiv1.SpinTransitionFromStateActive,
			ToState:   apiv1.SpinTransitionToStateStandby,
			At:        standby1,
		},
		{
			Device:               "/dev/sdb",
			FromState:            apiv1.SpinTransitionFromStateStandby,
			ToState:              apiv1.SpinTransitionToStateActive,
			At:                   wake1,
			AwakeDurationSeconds: apiv1.NewOptNilInt64(int64(standby1.Sub(wake1).Seconds())),
		},
		{
			Device:    "/dev/sdc",
			FromState: apiv1.SpinTransitionFromStateStandby,
			ToState:   apiv1.SpinTransitionToStateActive,
			At:        wakeYesterday,
		},
	}
	if scenario == "degraded" {
		events = append(events, apiv1.SpinTransition{
			Device:               "/dev/sdd",
			FromState:            apiv1.SpinTransitionFromStateStandby,
			ToState:              apiv1.SpinTransitionToStateActive,
			At:                   wake2.Add(30 * time.Minute),
			AwakeDurationSeconds: apiv1.NewOptNilInt64(180),
		})
	}

	daily := []apiv1.DailyWakeCount{
		{Device: "/dev/sdb", Date: today, Count: 2},
		{Device: "/dev/sdc", Date: yesterday, Count: 1},
	}
	if scenario == "degraded" {
		daily = append(daily, apiv1.DailyWakeCount{
			Device: "/dev/sdd",
			Date:   today,
			Count:  1,
		})
	}

	return &apiv1.WakeEventsResponse{Events: events, DailyWakeCounts: daily}
}

func (h *handler) ListWakeEvents(ctx context.Context) (*apiv1.WakeEventsResponse, error) {
	return mockWakeEvents(h.scenario), nil
}
