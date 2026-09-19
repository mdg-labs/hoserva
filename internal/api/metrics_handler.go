package api

import (
	"context"
	"log"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/store/metrics"
)

func (h *Handler) GetMetrics(ctx context.Context, params apiv1.GetMetricsParams) (*apiv1.MetricSeries, error) {
	if !params.From.Before(params.To) {
		return nil, &apiError{code: "invalid_window", statusCode: 400, message: "from must be before to"}
	}

	subject := ""
	if s, ok := params.Subject.Get(); ok {
		subject = s
	}

	resolution := metrics.ResolutionForWindow(params.To.Sub(params.From))
	series := &apiv1.MetricSeries{
		Metric:     params.Metric,
		Subject:    subject,
		Resolution: apiv1.MetricResolution(resolution),
		From:       params.From,
		To:         params.To,
		Points:     []apiv1.MetricPoint{},
	}

	if h.Metrics == nil {
		return series, nil
	}

	samples, err := h.Metrics.ValuesInRange(ctx, resolution, params.Metric, subject, params.From, params.To)
	if err != nil {
		log.Printf("metrics: reading %s/%s: %v", params.Metric, subject, err)
		return series, nil
	}

	points := make([]apiv1.MetricPoint, 0, len(samples))
	for _, sample := range samples {
		points = append(points, apiv1.MetricPoint{
			At:    sample.At,
			Value: sample.Value,
		})
	}
	series.Points = points
	return series, nil
}
