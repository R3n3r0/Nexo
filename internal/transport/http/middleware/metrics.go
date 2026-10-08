package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/metrics"
)

// MetricsDeps are the dependencies of the metrics middleware.
type MetricsDeps struct {
	// Registry creates the HTTP instruments (required).
	Registry metrics.Registry
	// Clock measures latencies (default wall clock).
	Clock clock.Clock
}

// MetricsOption configures the metrics middleware.
type MetricsOption func(*metricsOptions)

type metricsOptions struct {
	server string
}

// WithMetricsServer sets the value of the "server" label that tells the
// listeners apart ("s3", "console"; default "http").
func WithMetricsServer(name string) MetricsOption {
	return func(o *metricsOptions) {
		if name != "" {
			o.server = name
		}
	}
}

type httpMetrics struct {
	requests metrics.Counter
	duration metrics.Histogram
	inflight metrics.Gauge
	server   string
	clock    clock.Clock
}

var _ Middleware = (*httpMetrics)(nil)

// NewMetrics returns a middleware recording request count, duration and
// in-flight requests. Labels are low-cardinality on purpose: no path label.
// The instruments are shared by every listener of a registry and told
// apart by the "server" label.
func NewMetrics(deps MetricsDeps, opts ...MetricsOption) (Middleware, error) {
	if deps.Registry == nil {
		return nil, fmt.Errorf("%w: metrics middleware requires a registry", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	o := metricsOptions{server: "http"}
	for _, opt := range opts {
		opt(&o)
	}
	// Registration is construction-time work, not tied to a request.
	ctx := context.Background()
	requests, err := deps.Registry.Counter(ctx, metrics.Definition{
		Subsystem: "http", Name: "requests_total",
		Help:   "Total HTTP requests by server, method and status.",
		Labels: []string{"server", "method", "status"},
	})
	if err != nil {
		return nil, fmt.Errorf("metrics middleware: %w", err)
	}
	duration, err := deps.Registry.Histogram(ctx, metrics.Definition{
		Subsystem: "http", Name: "request_duration_seconds",
		Help:   "HTTP request latency by server and method.",
		Labels: []string{"server", "method"},
	})
	if err != nil {
		return nil, fmt.Errorf("metrics middleware: %w", err)
	}
	inflight, err := deps.Registry.Gauge(ctx, metrics.Definition{
		Subsystem: "http", Name: "requests_in_flight",
		Help:   "HTTP requests currently being served.",
		Labels: []string{"server"},
	})
	if err != nil {
		return nil, fmt.Errorf("metrics middleware: %w", err)
	}
	return &httpMetrics{
		requests: requests,
		duration: duration,
		inflight: inflight,
		server:   o.server,
		clock:    deps.Clock,
	}, nil
}

// Wrap implements Middleware.
func (m *httpMetrics) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		m.inflight.Add(ctx, 1, m.server)
		defer m.inflight.Add(ctx, -1, m.server)
		start := m.clock.Now()
		rw := wrapWriter(w)
		next.ServeHTTP(rw, r)
		method := r.Method
		if !knownMethod(method) {
			method = "OTHER"
		}
		m.requests.Add(ctx, 1, m.server, method, strconv.Itoa(rw.Status()))
		m.duration.Observe(ctx, m.clock.Now().Sub(start).Seconds(), m.server, method)
	})
}

func knownMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodOptions, http.MethodPatch:
		return true
	}
	return false
}
