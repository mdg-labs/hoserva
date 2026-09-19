package main

import (
	"context"
	"math"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/store/metrics"
)

func mockMetricSeries(metric, subject string, from, to time.Time) *apiv1.MetricSeries {
	resolution := metrics.ResolutionForWindow(to.Sub(from))
	series := &apiv1.MetricSeries{
		Metric:     metric,
		Subject:    subject,
		Resolution: apiv1.MetricResolution(resolution),
		From:       from,
		To:         to,
		Points:     []apiv1.MetricPoint{},
	}

	switch metric {
	case metrics.DiskThroughputBytesPerSec, metrics.NetworkThroughputBytesPerSec:
		series.Points = mockThroughputPoints(metric, from, to, resolution)
	default:
		return series
	}
	return series
}

func mockThroughputPoints(metric string, from, to time.Time, resolution metrics.Resolution) []apiv1.MetricPoint {
	step := time.Minute
	switch resolution {
	case metrics.Hourly:
		step = time.Hour
	case metrics.Daily:
		step = 24 * time.Hour
	}

	base := 50_000_000.0
	if metric == metrics.NetworkThroughputBytesPerSec {
		base = 12_000_000.0
	}

	var points []apiv1.MetricPoint
	at := from.UTC()
	for i := 0; at.Before(to) || at.Equal(to); i++ {
		wave := math.Sin(float64(i) / 6.0)
		points = append(points, apiv1.MetricPoint{
			At:    at,
			Value: base * (1 + 0.35*wave),
		})
		at = at.Add(step)
		if len(points) >= 60 {
			break
		}
	}
	return points
}

func (h *handler) GetMetrics(ctx context.Context, params apiv1.GetMetricsParams) (*apiv1.MetricSeries, error) {
	if !params.From.Before(params.To) {
		return nil, &mockError{code: "invalid_window", statusCode: 400, message: "from must be before to"}
	}
	if h.scenario == "fresh-install" {
		subject := ""
		if s, ok := params.Subject.Get(); ok {
			subject = s
		}
		return &apiv1.MetricSeries{
			Metric:     params.Metric,
			Subject:    subject,
			Resolution: apiv1.MetricResolution(metrics.ResolutionForWindow(params.To.Sub(params.From))),
			From:       params.From,
			To:         params.To,
			Points:     []apiv1.MetricPoint{},
		}, nil
	}

	subject := ""
	if s, ok := params.Subject.Get(); ok {
		subject = s
	}
	return mockMetricSeries(params.Metric, subject, params.From, params.To), nil
}
