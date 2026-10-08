package middleware

import (
	"net/http"
	"strings"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

type auditMiddleware struct {
	auditor audit.Service
	clock   clock.Clock
}

var _ Middleware = (*auditMiddleware)(nil)

// AuditOption configures the audit middleware.
type AuditOption func(*auditMiddleware)

// WithAuditClock sets the clock used for timestamps and durations.
func WithAuditClock(c clock.Clock) AuditOption {
	return func(m *auditMiddleware) {
		if c != nil {
			m.clock = c
		}
	}
}

// NewAudit returns a middleware recording one audit entry per request. It
// installs an operation holder so that the router can report the API name
// (httpx.SetOperationName) and reads the identity set by the auth
// middlewares, which must therefore run before it.
func NewAudit(auditor audit.Service, opts ...AuditOption) Middleware {
	m := &auditMiddleware{auditor: auditor, clock: clock.NewReal()}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Wrap implements Middleware.
func (m *auditMiddleware) Wrap(next http.Handler) http.Handler {
	if m.auditor == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := m.clock.Now()
		ctx, holder := httpx.WithOperationHolder(r.Context())
		rw := wrapWriter(w)
		next.ServeHTTP(rw, r.WithContext(ctx))

		bucket, object := splitPath(r.URL.Path)
		entry := audit.Entry{
			Version:    audit.EntryVersion,
			Time:       start,
			RequestID:  httpx.RequestIDFrom(r.Context()),
			RemoteHost: remoteHost(r.RemoteAddr),
			UserAgent:  r.UserAgent(),
			API: audit.API{
				Name:           holder.Name(),
				Bucket:         bucket,
				Object:         object,
				Status:         http.StatusText(rw.Status()),
				StatusCode:     rw.Status(),
				InputBytes:     max(r.ContentLength, 0),
				OutputBytes:    rw.bytes,
				TimeToResponse: m.clock.Now().Sub(start),
			},
			ReqQuery: flatten(r.URL.Query()),
		}
		if entry.API.Name == "" {
			entry.API.Name = r.Method + " " + r.URL.Path
		}
		if id, ok := IdentityFromContext(ctx); ok {
			entry.AccessKey = id.AccessKey
			entry.ParentUser = id.Parent
		}
		_ = m.auditor.Log(ctx, entry)
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

func remoteHost(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 && !strings.HasSuffix(addr, "]") {
		return strings.Trim(addr[:i], "[]")
	}
	return addr
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
