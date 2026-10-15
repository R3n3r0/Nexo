package s3

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// MetricsDeps are the dependencies of the metrics routes. Registry is
// required; unless the routes are public (WithPublicMetrics), Verifier
// (bearer tokens) and Policy (admin:Prometheus) are required too. Enc and
// Clock default to httpx.New() and the wall clock.
type MetricsDeps struct {
	Registry metrics.Registry
	// Exporter gathers the families of each scope (the node, every node
	// of the cluster, the buckets); without it every scope serves the
	// whole registry of the node.
	Exporter metrics.Exporter
	Verifier auth.Verifier
	Policy   policy.Service
	Enc      httpx.Encoder
	Clock    clock.Clock
}

type metricsRoutes struct {
	deps    MetricsDeps
	public  bool
	handler http.Handler
}

var _ MetricsRoutes = (*metricsRoutes)(nil)

// Metrics endpoint paths.
const (
	// MetricsPathPrefix prefixes every metrics path. The routes
	// authenticate their own requests (bearer tokens), so the SigV4
	// middleware must skip it (middleware.AuthDeps.SkipPaths).
	MetricsPathPrefix  = "/nexo/metrics/"
	PathMetricsCluster = MetricsPathPrefix + "v1/cluster"
	PathMetricsNode    = MetricsPathPrefix + "v1/node"
	PathMetricsBucket  = MetricsPathPrefix + "v1/bucket"
)

// bearerPrefix is the authentication scheme of the scrape tokens.
const bearerPrefix = "Bearer "

// WithPublicMetrics serves the metrics endpoints without authentication
// (NEXO_PROMETHEUS_AUTH_TYPE=public). By default they require a bearer
// token of an identity allowed admin:Prometheus. Used by NewMetricsRoutes.
func WithPublicMetrics(public bool) Option {
	return func(o *options) { o.publicMetrics = public }
}

// NewMetricsRoutes returns the metrics route group (Prometheus text
// exposition, served by the registry itself). The registry is wired by the
// composition root (the same one the middlewares write to). A missing
// dependency is a wiring error (domain.ErrInvalidArgument). With an
// exporter each scope serves its families (docs/07 §7.4).
func NewMetricsRoutes(deps MetricsDeps, opts ...Option) (MetricsRoutes, error) {
	o := newOptions(opts)
	if deps.Registry == nil {
		return nil, fmt.Errorf("%w: s3: metrics routes require a registry", domain.ErrInvalidArgument)
	}
	if !o.publicMetrics && (deps.Verifier == nil || deps.Policy == nil) {
		return nil, fmt.Errorf("%w: s3: protected metrics routes require a verifier and a policy engine", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	return &metricsRoutes{deps: deps, public: o.publicMetrics, handler: deps.Registry.Handler()}, nil
}

// Register implements Routes.
func (h *metricsRoutes) Register(r chi.Router) {
	r.Get(PathMetricsCluster, h.serve(metrics.ScopeCluster))
	r.Get(PathMetricsNode, h.serve(metrics.ScopeNode))
	r.Get(PathMetricsBucket, h.serve(metrics.ScopeBucket))
}

// serve authenticates and authorizes the scraper, then answers the
// families of scope (the exporter), or the whole registry.
func (h *metricsRoutes) serve(scope metrics.Scope) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.public {
			if err := h.authorize(r); err != nil {
				h.deps.Enc.WriteS3Error(w, r, err)
				return
			}
		}
		if h.deps.Exporter == nil {
			h.handler.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		families, err := h.deps.Exporter.Gather(ctx, scope)
		if err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
		w.Header().Set("Content-Type", metrics.TextContentType)
		_ = h.deps.Exporter.WriteText(ctx, w, families)
	}
}

// authorize verifies the bearer token and asks the policy engine for
// admin:Prometheus.
func (h *metricsRoutes) authorize(r *http.Request) error {
	ctx := r.Context()
	header := r.Header.Get("Authorization")
	if len(header) <= len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return domain.NewS3Error(domain.ErrAccessDenied, "AccessDenied", "The metrics endpoints require a bearer token.")
	}
	id, err := h.deps.Verifier.VerifyBearerToken(ctx, strings.TrimSpace(header[len(bearerPrefix):]))
	if err != nil {
		return err
	}
	decision, err := h.deps.Policy.Evaluate(ctx, policy.Args{Identity: id, Action: policy.ActionAdminPrometheus, Now: h.deps.Clock.Now()})
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return fmt.Errorf("%w: %s", domain.ErrAccessDenied, decision.Reason)
	}
	return nil
}
