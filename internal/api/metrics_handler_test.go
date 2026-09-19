package api_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/store/metrics"
)

func TestHandler_GetMetrics_NilMetricsReturnsEmpty(t *testing.T) {
	h := &api.Handler{}
	from := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)

	resp, err := h.GetMetrics(context.Background(), apiv1.GetMetricsParams{
		Metric: metrics.DiskThroughputBytesPerSec,
		From:   from,
		To:     to,
	})
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if len(resp.Points) != 0 {
		t.Fatalf("len(points) = %d, want 0", len(resp.Points))
	}
	if resp.Resolution != apiv1.MetricResolutionRaw {
		t.Fatalf("resolution = %q, want raw for a one-hour window", resp.Resolution)
	}
}

func TestHandler_GetMetrics_InvalidWindow(t *testing.T) {
	h := &api.Handler{}
	at := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

	_, err := h.GetMetrics(context.Background(), apiv1.GetMetricsParams{
		Metric: metrics.DiskThroughputBytesPerSec,
		From:   at,
		To:     at,
	})
	if err == nil {
		t.Fatal("expected error for from == to")
	}
}

func newMetricsTestHandler(t *testing.T) (*api.Handler, *metrics.Store) {
	t.Helper()
	s, err := metrics.Open(context.Background(), filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &api.Handler{Metrics: s}, s
}

func TestHandler_GetMetrics_FromPersistedSamples(t *testing.T) {
	ctx := context.Background()
	h, store := newMetricsTestHandler(t)
	from := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	at := from.Add(15 * time.Minute)
	to := from.Add(time.Hour)

	if err := store.Insert(ctx, metrics.Sample{
		Metric: metrics.NetworkThroughputBytesPerSec,
		At:     at,
		Value:  42,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	resp, err := h.GetMetrics(ctx, apiv1.GetMetricsParams{
		Metric: metrics.NetworkThroughputBytesPerSec,
		From:   from,
		To:     to,
	})
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if len(resp.Points) != 1 {
		t.Fatalf("len(points) = %d, want 1", len(resp.Points))
	}
	if resp.Points[0].Value != 42 {
		t.Fatalf("value = %v, want 42", resp.Points[0].Value)
	}
}

func TestHandler_GetMetrics_HourlyResolutionForLongWindow(t *testing.T) {
	h := &api.Handler{}
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)

	resp, err := h.GetMetrics(context.Background(), apiv1.GetMetricsParams{
		Metric: metrics.DiskThroughputBytesPerSec,
		From:   from,
		To:     to,
	})
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if resp.Resolution != apiv1.MetricResolutionHourly {
		t.Fatalf("resolution = %q, want hourly", resp.Resolution)
	}
}
