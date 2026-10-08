package s3

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/r3n3r0/nexo/internal/domain"
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

// NewMetricsRoutes returns the Prometheus route group. The registry is
// wired by the composition root (the same one the middlewares write to);
// a nil registry is a wiring error. The three scopes serve the same registry
// until per-scope collectors exist (M8).
func NewMetricsRoutes(deps MetricsDeps) (MetricsRoutes, error) {
	if deps.Registry == nil {
		return nil, fmt.Errorf("%w: s3: metrics routes require a registry", domain.ErrInvalidArgument)
	}
	return &metricsRoutes{
		deps: deps,
		handler: promhttp.HandlerFor(deps.Registry, promhttp.HandlerOpts{
			ErrorHandling: promhttp.ContinueOnError,
		}),
	}, nil
}

// Register implements Routes.
func (h *metricsRoutes) Register(r chi.Router) {
	r.Get(PathMetricsCluster, h.handler.ServeHTTP)
	r.Get(PathMetricsNode, h.handler.ServeHTTP)
	r.Get(PathMetricsBucket, h.handler.ServeHTTP)
}
