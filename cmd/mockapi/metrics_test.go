package main

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/store/metrics"
)

func TestGetMetrics_HealthyScenarioIsNonEmpty(t *testing.T) {
	client := newTestClient(t, "healthy")
	now := time.Now().UTC()

	resp, err := client.GetMetrics(context.Background(), apiv1.GetMetricsParams{
		Metric: metrics.DiskThroughputBytesPerSec,
		From:   now.Add(-time.Hour),
		To:     now,
	})
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if len(resp.Points) == 0 {
		t.Fatal("healthy scenario should serve a non-empty throughput series")
	}
}

func TestGetMetrics_FreshInstallIsEmpty(t *testing.T) {
	client := newTestClient(t, "fresh-install")
	now := time.Now().UTC()

	resp, err := client.GetMetrics(context.Background(), apiv1.GetMetricsParams{
		Metric: metrics.DiskThroughputBytesPerSec,
		From:   now.Add(-time.Hour),
		To:     now,
	})
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if len(resp.Points) != 0 {
		t.Fatalf("fresh-install should be empty, got %d points", len(resp.Points))
	}
}
