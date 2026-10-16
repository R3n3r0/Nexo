package middleware

import (
	"fmt"
	"net/http"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/trace"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// TraceDeps are the dependencies of the live trace middleware.
type TraceDeps struct {
	// Tracer receives the events (required).
	Tracer trace.Service
	// Clock measures durations (default wall clock).
	Clock clock.Clock
}

// TraceOption configures the live trace middleware.
type TraceOption func(*traceOptions)

type traceOptions struct {
	typ     trace.Type
	only    func(r *http.Request) bool
	api     func(r *http.Request) string
	domains []string
}

// WithTraceType sets the type of the events (default trace.TypeS3).
func WithTraceType(t trace.Type) TraceOption {
	return func(o *traceOptions) {
		if t != "" {
			o.typ = t
		}
	}
}

// WithTraceOnly traces the requests only selects (the internode calls
// among the requests of the S3 port, the API of the console).
func WithTraceOnly(only func(r *http.Request) bool) TraceOption {
	return func(o *traceOptions) { o.only = only }
}

// WithTraceAPI names the operation of the requests no router names (the
// internode calls: their method).
func WithTraceAPI(api func(r *http.Request) string) TraceOption {
	return func(o *traceOptions) { o.api = api }
}

// WithTraceDomains names the bucket of the virtual-host requests.
func WithTraceDomains(domains ...string) TraceOption {
	return func(o *traceOptions) { o.domains = domains }
}

type traceMiddleware struct {
	deps TraceDeps
	opts traceOptions
}

var _ Middleware = (*traceMiddleware)(nil)

// NewTrace returns the middleware publishing every request to the live
// trace (S-OPS-07) while someone subscribes: method, path, query (the
// signatures, credentials and tokens redacted), status, duration, sizes,
// request id, client, caller and bucket. No header and no body is
// published. Without a subscriber the requests pass through untouched. A
// missing tracer is a wiring error (domain.ErrInvalidArgument).
func NewTrace(deps TraceDeps, opts ...TraceOption) (Middleware, error) {
	if deps.Tracer == nil {
		return nil, fmt.Errorf("%w: trace middleware requires the tracer", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	o := traceOptions{typ: trace.TypeS3}
	for _, opt := range opts {
		opt(&o)
	}
	return &traceMiddleware{deps: deps, opts: o}, nil
}

// Wrap implements Middleware.
func (m *traceMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.deps.Tracer.Enabled() || (m.opts.only != nil && !m.opts.only(r)) {
			next.ServeHTTP(w, r)
			return
		}
		start := m.deps.Clock.Now()
		ctx, holder := observe(r.Context())
		r = r.WithContext(ctx)
		received := countBody(r)
		rw := wrapWriter(w)
		next.ServeHTTP(rw, r)

		e := trace.Event{
			Time:        start,
			Type:        m.opts.typ,
			API:         holder.Name(),
			Method:      r.Method,
			Path:        r.URL.Path,
			Query:       httpx.RedactRawQuery(r.URL.RawQuery),
			Status:      rw.Status(),
			Duration:    m.deps.Clock.Now().Sub(start),
			InputBytes:  received(),
			OutputBytes: rw.bytes,
			RequestID:   requestIDOf(ctx, rw),
			ClientIP:    ClientOf(r).IP,
			Error:       outcomeError(rw.Status(), holder.Throttled()),
		}
		if e.API == "" && m.opts.api != nil {
			e.API = m.opts.api(r)
		}
		switch m.opts.typ {
		case trace.TypeAdmin:
			e.Bucket = adminBucket(r.URL.Path)
		case trace.TypeS3:
			e.Bucket, _ = targetOf(r, m.opts.domains)
		}
		if id, ok := callerOf(ctx); ok && id != nil {
			e.User = id.AccessKey
		}
		m.deps.Tracer.Publish(ctx, e)
	})
}
