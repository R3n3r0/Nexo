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

type auditMiddleware struct {
	deps AuditDeps
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
func NewAudit(deps AuditDeps) (Middleware, error) {
	if deps.Auditor == nil {
		return nil, fmt.Errorf("%w: audit middleware requires an auditor", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	return &auditMiddleware{deps: deps}, nil
}

// Wrap implements Middleware.
func (m *auditMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := m.deps.Clock.Now()
		ctx, holder := httpx.WithOperationHolder(r.Context())
		ctx = WithIdentityHolder(ctx)
		rw := wrapWriter(w)
		next.ServeHTTP(rw, r.WithContext(ctx))

		bucket, object := splitPath(r.URL.Path)
		entry := audit.Entry{
			Version:    audit.EntryVersion,
			Time:       start,
			RequestID:  httpx.RequestIDFrom(r.Context()),
			RemoteHost: ClientOf(r).IP,
			UserAgent:  r.UserAgent(),
			API: audit.API{
				Name:           holder.Name(),
				Bucket:         bucket,
				Object:         object,
				Status:         http.StatusText(rw.Status()),
				StatusCode:     rw.Status(),
				InputBytes:     max(r.ContentLength, 0),
				OutputBytes:    rw.bytes,
				TimeToResponse: m.deps.Clock.Now().Sub(start),
			},
			// Presigned signatures and tokens are capabilities: never logged.
			ReqQuery: flatten(httpx.RedactQuery(r.URL.Query())),
		}
		if entry.API.Name == "" {
			entry.API.Name = r.Method + " " + r.URL.Path
		}
		id, ok := HeldIdentity(ctx)
		if !ok { // authenticated before this middleware
			id, ok = IdentityFromContext(ctx)
		}
		if ok {
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
