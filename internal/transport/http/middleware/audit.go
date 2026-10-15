package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// AuditDeps are the dependencies of the audit middleware.
type AuditDeps struct {
	// Auditor records the entries (required).
	Auditor audit.Service
	// Clock timestamps the entries and measures durations (default wall clock).
	Clock clock.Clock
}

// AuditOption configures the audit middleware.
type AuditOption func(*auditOptions)

type auditOptions struct {
	typ     string
	domains []string
	only    func(r *http.Request) bool
}

// Types of the audited requests (audit.Entry.Type).
const (
	AuditTypeS3    = "s3"
	AuditTypeAdmin = "admin"
)

// WithAuditType sets the type of the entries (default AuditTypeS3); the
// entries of the Admin API (AuditTypeAdmin) name the bucket of their path.
func WithAuditType(t string) AuditOption {
	return func(o *auditOptions) {
		if t != "" {
			o.typ = t
		}
	}
}

// WithAuditDomains names the bucket of the virtual-host requests
// ("<bucket>.<domain>").
func WithAuditDomains(domains ...string) AuditOption {
	return func(o *auditOptions) { o.domains = domains }
}

// WithAuditOnly audits the requests only selects (default: every
// request); the console audits its API, not the assets of the web UI.
func WithAuditOnly(only func(r *http.Request) bool) AuditOption {
	return func(o *auditOptions) { o.only = only }
}

type auditMiddleware struct {
	deps AuditDeps
	opts auditOptions
}

var _ Middleware = (*auditMiddleware)(nil)

// NewAudit returns a middleware recording one audit entry per request. It
// installs an operation holder, so that the router can report the API name
// (httpx.SetOperationName), and an identity holder, so that the identity
// established below it is recorded: the one of the auth middleware or the
// one a handler authenticates itself (presigned URLs, POST policy). It must
// run before (outside) the auth middleware, so that the requests rejected
// by the authentication are recorded too. A missing auditor is a wiring
// error (domain.ErrInvalidArgument).
func NewAudit(deps AuditDeps, opts ...AuditOption) (Middleware, error) {
	if deps.Auditor == nil {
		return nil, fmt.Errorf("%w: audit middleware requires an auditor", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	o := auditOptions{typ: AuditTypeS3}
	for _, opt := range opts {
		opt(&o)
	}
	return &auditMiddleware{deps: deps, opts: o}, nil
}

// Wrap implements Middleware.
func (m *auditMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.opts.only != nil && !m.opts.only(r) {
			next.ServeHTTP(w, r)
			return
		}
		start := m.deps.Clock.Now()
		ctx, holder := observe(r.Context())
		rw := wrapWriter(w)
		r = r.WithContext(ctx)
		received := countBody(r)
		next.ServeHTTP(rw, r)

		bucket, object := targetOf(r, m.opts.domains)
		if m.opts.typ == AuditTypeAdmin {
			bucket, object = adminBucket(r.URL.Path), r.URL.Query().Get("key")
		}
		if id, ok := callerOf(ctx); bucket == "" && probe(r, rw.Status(), id, ok) {
			// The health probes of the load balancers and of Kubernetes,
			// and the anonymous scrapes: every few seconds per node, they
			// would push the requests of the users out of the ring.
			return
		}
		entry := audit.Entry{
			Version:    audit.EntryVersion,
			Type:       m.opts.typ,
			Time:       start,
			RequestID:  requestIDOf(ctx, rw),
			RemoteHost: ClientOf(r).IP,
			UserAgent:  r.UserAgent(),
			API: audit.API{
				Name:           holder.Name(),
				Method:         r.Method,
				Path:           r.URL.Path,
				Bucket:         bucket,
				Object:         object,
				Status:         http.StatusText(rw.Status()),
				StatusCode:     rw.Status(),
				InputBytes:     received(),
				OutputBytes:    rw.bytes,
				TimeToResponse: m.deps.Clock.Now().Sub(start),
			},
			// Presigned signatures and tokens are capabilities: never logged.
			ReqQuery: flatten(httpx.RedactQuery(r.URL.Query())),
			Error:    statusError(rw.Status()),
		}
		if entry.API.Name == "" {
			entry.API.Name = r.Method + " " + r.URL.Path
		}
		if id, ok := callerOf(ctx); ok {
			entry.AccessKey = id.AccessKey
			entry.ParentUser = id.Parent
		}
		_ = m.deps.Auditor.Log(ctx, entry)
	})
}

// splitPath extracts the bucket and object of a path-style S3 URL.
func splitPath(p string) (bucket, object string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" || strings.HasPrefix(p, "nexo/") || strings.HasPrefix(p, "api/") {
		return "", ""
	}
	bucket, object, _ = strings.Cut(p, "/")
	return bucket, object
}

func flatten(values map[string][]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = strings.Join(v, ",")
	}
	return out
}

// probePrefixes are the paths of the probes the audit leaves out: the
// health checks and the metrics scrapes, when anonymous.
const (
	healthPrefix  = "/nexo/health/"
	metricsPrefix = "/nexo/metrics/"
)

// probe reports a request the audit does not record, outside any bucket:
// an anonymous health probe, whatever the health it reports, or an
// anonymous metrics scrape served (NEXO_PROMETHEUS_AUTH_TYPE=public). A
// request that presents credentials (a signature, a bearer token: an
// attempt to authenticate, valid or not) and a refused scrape are recorded
// like any other.
func probe(r *http.Request, status int, id *domain.Identity, ok bool) bool {
	if authenticated(id, ok) || presentsCredentials(r) {
		return false
	}
	switch {
	case strings.HasPrefix(r.URL.Path, healthPrefix):
		return true
	case strings.HasPrefix(r.URL.Path, metricsPrefix):
		return status < http.StatusBadRequest
	}
	return false
}

// credentialParams are the query parameters of the presigned and
// signature V2 credentials.
var credentialParams = []string{"X-Amz-Credential", "X-Amz-Signature", "AWSAccessKeyId", "Signature"}

// presentsCredentials reports a request carrying credentials: an
// Authorization header (SigV4, bearer token) or a presigned signature.
func presentsCredentials(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return true
	}
	q := r.URL.Query()
	for _, k := range credentialParams {
		if q.Has(k) {
			return true
		}
	}
	return false
}
