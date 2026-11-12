package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/spj/sindri/internal/version"
)

// queueStates are the sindri_queue_jobs label values, fixed so that an alert
// sees 0 rather than a missing series when a state has no jobs.
var queueStates = []string{"queued", "running", "failed"}

// findingSeverities are the sindri_approved_findings label values, fixed so
// that an alert sees 0 rather than a missing series (ADR 31).
var findingSeverities = []string{"CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"}

// metrics holds the instruments the HTTP layer reports.
type metrics struct {
	registry    *prometheus.Registry
	requests    *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	queueJobs   *prometheus.GaugeVec
	queueCounts func(ctx context.Context) (map[string]int64, error)

	approvedFindings      *prometheus.GaugeVec
	approvedFindingCounts func(ctx context.Context) (map[string]int64, error)
}

// newMetrics registers Sindri's instruments on registry.
//
// An explicit registry rather than the global default: a global registry makes
// two servers in one process collide, which is exactly what an integration test
// does.
func newMetrics(registry *prometheus.Registry, queueCounts, approvedFindingCounts func(ctx context.Context) (map[string]int64, error)) (*metrics, error) {
	m := &metrics{
		registry:              registry,
		queueCounts:           queueCounts,
		approvedFindingCounts: approvedFindingCounts,
		approvedFindings: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sindri_approved_findings",
			Help: "Currently approved subjects whose newest Sindri scan has a finding of this severity, read at scrape time.",
		}, []string{"severity"}),
		queueJobs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sindri_queue_jobs",
			Help: "Background jobs by state, read from the queue table at scrape time.",
		}, []string{"state"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sindri_http_requests_total",
			Help: "HTTP requests by method, matched route, and status.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "sindri_http_request_duration_seconds",
			Help: "HTTP request duration by method and matched route.",
			// Wide, because a health check and a gigabyte layer pull share
			// this histogram.
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60},
		}, []string{"method", "route"}),
	}

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sindri_build_info",
		Help: "Build metadata of the running binary, always 1.",
	}, []string{"version", "commit"})
	buildInfo.WithLabelValues(version.Version, version.Commit).Set(1)

	for _, c := range []prometheus.Collector{
		m.requests,
		m.duration,
		m.queueJobs,
		m.approvedFindings,
		buildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := registry.Register(c); err != nil {
			return nil, fmt.Errorf("registering metrics: %w", err)
		}
	}

	return m, nil
}

func (m *metrics) record(method, route string, status int, took time.Duration) {
	m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(method, route).Observe(took.Seconds())
}

func (m *metrics) handler() http.Handler {
	serve := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.refreshQueue(r.Context())
		m.refreshApprovedFindings(r.Context())
		serve.ServeHTTP(w, r)
	})
}

// refreshQueue reads the queue here rather than in a Collector, because a
// Collector is given no context to do I/O with.
//
// When the read fails the series are dropped: a stale number would tell an
// alert that all is well, and the rest of the scrape is still worth having.
func (m *metrics) refreshQueue(ctx context.Context) {
	if m.queueCounts == nil {
		return
	}
	counts, err := m.queueCounts(ctx)
	if err != nil {
		m.queueJobs.Reset()
		LoggerFromContext(ctx).WarnContext(ctx, "metrics: reading the job queue failed",
			slog.String("error", err.Error()))
		return
	}
	for _, state := range queueStates {
		m.queueJobs.WithLabelValues(state).Set(float64(counts[state]))
	}
}

// refreshApprovedFindings reads the approved-findings counts at scrape time,
// like refreshQueue. Without a source (scanning is off) the gauge has no
// series; a failed read drops them rather than serve a stale number.
func (m *metrics) refreshApprovedFindings(ctx context.Context) {
	if m.approvedFindingCounts == nil {
		return
	}
	counts, err := m.approvedFindingCounts(ctx)
	if err != nil {
		m.approvedFindings.Reset()
		LoggerFromContext(ctx).WarnContext(ctx, "metrics: reading approved findings failed",
			slog.String("error", err.Error()))
		return
	}
	for _, sev := range findingSeverities {
		m.approvedFindings.WithLabelValues(sev).Set(float64(counts[sev]))
	}
}
