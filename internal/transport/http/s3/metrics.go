package s3

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/r3n3r0/nexo/internal/service/metrics"
)

// MetricsDeps are the dependencies of the metrics routes.
type MetricsDeps struct {
	Registry metrics.Registry
}

type metricsRoutes struct {
	deps    MetricsDeps
	handler http.Handler
}

var _ MetricsRoutes = (*metricsRoutes)(nil)

// Metrics endpoint paths.
const (
	PathMetricsCluster = "/nexo/metrics/v1/cluster"
	PathMetricsNode    = "/nexo/metrics/v1/node"
	PathMetricsBucket  = "/nexo/metrics/v1/bucket"
)

// NewMetricsRoutes returns the Prometheus route group. The three scopes
// serve the same registry until per-scope collectors exist (TODO M8).
func NewMetricsRoutes(deps MetricsDeps) MetricsRoutes {
	if deps.Registry == nil {
		deps.Registry = metrics.New(metrics.Deps{})
	}
	return &metricsRoutes{
		deps: deps,
		handler: promhttp.HandlerFor(deps.Registry, promhttp.HandlerOpts{
			ErrorHandling: promhttp.ContinueOnError,
		}),
	}
}

// Register implements Routes.
func (h *metricsRoutes) Register(r chi.Router) {
	r.Get(PathMetricsCluster, h.handler.ServeHTTP)
	r.Get(PathMetricsNode, h.handler.ServeHTTP)
	r.Get(PathMetricsBucket, h.handler.ServeHTTP)
}
