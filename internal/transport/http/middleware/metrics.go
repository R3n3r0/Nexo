package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/metrics"
)

type httpMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight prometheus.Gauge
	server   string
	clock    clock.Clock
}

var _ Middleware = (*httpMetrics)(nil)

// MetricsOption configures the metrics middleware.
type MetricsOption func(*httpMetrics)

// WithMetricsClock sets the clock used to measure latencies (tests).
func WithMetricsClock(c clock.Clock) MetricsOption {
	return func(m *httpMetrics) {
		if c != nil {
			m.clock = c
		}
	}
}

// NewMetrics returns a middleware recording request count, duration and
// in-flight requests in reg. server labels the listener ("s3", "console").
// Labels are low-cardinality on purpose: no path label.
func NewMetrics(reg metrics.Registry, server string, opts ...MetricsOption) (Middleware, error) {
	m := &httpMetrics{
		server: server,
		clock:  clock.NewReal(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metrics.Namespace, Subsystem: "http", Name: "requests_total",
			Help: "Total HTTP requests by server, method and status.",
		}, []string{"server", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metrics.Namespace, Subsystem: "http", Name: "request_duration_seconds",
			Help:    "HTTP request latency by server and method.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"server", "method"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metrics.Namespace, Subsystem: "http", Name: "requests_in_flight",
			Help:        "HTTP requests currently being served.",
			ConstLabels: prometheus.Labels{"server": server},
		}),
	}
	for _, o := range opts {
		o(m)
	}
	for _, c := range []prometheus.Collector{m.requests, m.duration, m.inflight} {
		if err := reg.Register(c); err != nil {
			var already prometheus.AlreadyRegisteredError
			if !errors.As(err, &already) {
				return nil, fmt.Errorf("metrics middleware: register: %w", err)
			}
		}
	}
	return m, nil
}

// Wrap implements Middleware.
func (m *httpMetrics) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.inflight.Inc()
		defer m.inflight.Dec()
		start := m.clock.Now()
		rw := wrapWriter(w)
		next.ServeHTTP(rw, r)
		method := r.Method
		if !knownMethod(method) {
			method = "OTHER"
		}
		m.requests.WithLabelValues(m.server, method, strconv.Itoa(rw.Status())).Inc()
		m.duration.WithLabelValues(m.server, method).Observe(m.clock.Now().Sub(start).Seconds())
	})
}

func knownMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodOptions, http.MethodPatch:
		return true
	}
	return false
}
