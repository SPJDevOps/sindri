package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/worker"
)

// webhookMetrics counts webhook delivery attempts and times them. The notify
// package stays free of Prometheus: the handler is wrapped here, where the
// registry the server serves is known.
type webhookMetrics struct {
	total    *prometheus.CounterVec
	duration prometheus.Histogram
}

// newWebhookMetrics registers the webhook instruments on reg. Every result
// label starts at zero, so an alert sees 0 rather than a missing series.
func newWebhookMetrics(reg prometheus.Registerer) (*webhookMetrics, error) {
	m := &webhookMetrics{
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sindri_webhook_deliveries_total",
			Help: "Webhook delivery attempts by result: delivered, retried (it will be tried again), or failed (no more attempts).",
		}, []string{"result"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sindri_webhook_delivery_duration_seconds",
			Help:    "Duration of webhook delivery attempts.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}),
	}
	for _, c := range []prometheus.Collector{m.total, m.duration} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("registering webhook metrics: %w", err)
		}
	}
	for _, r := range []string{"delivered", "retried", "failed"} {
		m.total.WithLabelValues(r)
	}
	return m, nil
}

// wrap records every attempt of h. An error is final when it is permanent or
// when the job has no attempts left, because the queue then stops retrying.
func (m *webhookMetrics) wrap(h worker.Handler) worker.Handler {
	return func(ctx context.Context, job queue.Job) (json.RawMessage, error) {
		start := time.Now()
		res, err := h(ctx, job)
		m.duration.Observe(time.Since(start).Seconds())
		m.total.WithLabelValues(webhookResult(job, err)).Inc()
		return res, err
	}
}

func webhookResult(job queue.Job, err error) string {
	switch {
	case err == nil:
		return "delivered"
	case errors.Is(err, worker.ErrPermanent), job.Attempts >= job.MaxAttempts:
		return "failed"
	default:
		return "retried"
	}
}
