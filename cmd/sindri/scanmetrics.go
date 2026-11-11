package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/worker"
)

// scanMetrics counts scan jobs and times them. The scan package stays free of
// Prometheus: the handler is wrapped here, where the registry the server
// serves is known.
type scanMetrics struct {
	total    *prometheus.CounterVec
	duration prometheus.Histogram
}

// newScanMetrics registers the scan instruments on reg. Every result label
// starts at zero, so an alert sees 0 rather than a missing series.
func newScanMetrics(reg prometheus.Registerer) (*scanMetrics, error) {
	m := &scanMetrics{
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sindri_scans_total",
			Help: "Scan job attempts by result: succeeded, failed, or skipped (nothing scannable).",
		}, []string{"result"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "sindri_scan_duration_seconds",
			Help: "Duration of scan job attempts, including the vulnerability database update.",
			// A small wheel takes seconds; a large image with a cold database,
			// minutes.
			Buckets: []float64{1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800},
		}),
	}
	for _, c := range []prometheus.Collector{m.total, m.duration} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("registering scan metrics: %w", err)
		}
	}
	for _, r := range []string{"succeeded", "failed", "skipped"} {
		m.total.WithLabelValues(r)
	}
	return m, nil
}

// wrap records every attempt of h. A result that says "skipped" with no error
// is the scanner declining to scan, which is not a success to alert on.
func (m *scanMetrics) wrap(h worker.Handler) worker.Handler {
	return func(ctx context.Context, job queue.Job) (json.RawMessage, error) {
		start := time.Now()
		res, err := h(ctx, job)
		m.duration.Observe(time.Since(start).Seconds())
		var skipped struct {
			Skipped string `json:"skipped"`
		}
		switch {
		case err != nil:
			m.total.WithLabelValues("failed").Inc()
		case json.Unmarshal(res, &skipped) == nil && skipped.Skipped != "":
			m.total.WithLabelValues("skipped").Inc()
		default:
			m.total.WithLabelValues("succeeded").Inc()
		}
		return res, err
	}
}
