package server

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/spj/sindri/internal/version"
)

// metrics holds the instruments the HTTP layer reports.
type metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// newMetrics registers Sindri's instruments on registry.
//
// An explicit registry rather than the global default: a global registry makes
// two servers in one process collide, which is exactly what an integration test
// does.
func newMetrics(registry *prometheus.Registry) (*metrics, error) {
	m := &metrics{
		registry: registry,
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
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
