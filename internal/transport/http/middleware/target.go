package middleware

import (
	"net/http"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// TargetDeps are the dependencies of the target middleware.
type TargetDeps struct {
	// HostBuckets resolves the custom hosts of the buckets (the aliases of
	// the s3 distributions the listener serves), like the S3 API; nil
	// resolves none.
	HostBuckets httpx.HostBucketResolver
}

// TargetOption configures the target middleware.
type TargetOption func(*target)

// WithTargetDomains sets the virtual-host domains (NEXO_DOMAIN).
func WithTargetDomains(domains ...string) TargetOption {
	return func(t *target) { t.domains = append(t.domains, domains...) }
}

type target struct {
	deps    TargetDeps
	domains []string
}

var _ Middleware = (*target)(nil)

// NewTarget returns the middleware that records the bucket and object of
// an S3 request before it is authenticated or routed, the way the S3 API
// resolves them (custom host of an s3 distribution, virtual host, path):
// the audit, the metrics and the traces of a request refused before its
// handler report the right bucket. The handler that serves the request
// records the target it actually used (httpx.SetOperationTarget).
func NewTarget(deps TargetDeps, opts ...TargetOption) Middleware {
	t := &target{deps: deps}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Wrap implements Middleware.
func (t *target) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := holdOperation(r.Context())
		r = r.WithContext(ctx)
		bucket, object := targetOf(r, t.domains)
		if t.deps.HostBuckets != nil && !strings.HasPrefix(r.URL.Path, domain.SystemPathPrefix) {
			if b, ok := t.deps.HostBuckets.BucketOfHost(HostOf(r)); ok {
				bucket, object = b, strings.TrimPrefix(r.URL.Path, "/")
			}
		}
		httpx.SetOperationTarget(ctx, bucket, object)
		next.ServeHTTP(w, r)
	})
}
