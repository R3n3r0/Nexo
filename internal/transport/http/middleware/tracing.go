package middleware

import (
	"fmt"
	"net/http"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/trace"
)

// TracingDeps are the dependencies of the distributed tracing middleware.
type TracingDeps struct {
	// Telemetry records the spans (required).
	Telemetry trace.Telemetry
}

// TracingOption configures the distributed tracing middleware.
type TracingOption func(*tracingOptions)

type tracingOptions struct {
	server  string
	only    func(r *http.Request) bool
	api     func(r *http.Request) string
	domains []string
}

// WithTracingServer names the listener in the spans ("s3", "console",
// "web", "rpc"; default "http").
func WithTracingServer(name string) TracingOption {
	return func(o *tracingOptions) {
		if name != "" {
			o.server = name
		}
	}
}

// WithTracingOnly traces the requests only selects.
func WithTracingOnly(only func(r *http.Request) bool) TracingOption {
	return func(o *tracingOptions) { o.only = only }
}

// WithTracingAPI names the operation of the requests no router names.
func WithTracingAPI(api func(r *http.Request) string) TracingOption {
	return func(o *tracingOptions) { o.api = api }
}

// WithTracingDomains names the bucket of the virtual-host requests.
func WithTracingDomains(domains ...string) TracingOption {
	return func(o *tracingOptions) { o.domains = domains }
}

type tracing struct {
	deps TracingDeps
	opts tracingOptions
}

var _ Middleware = (*tracing)(nil)

// NewTracing returns the middleware of the distributed tracing (docs/05
// T-8.5): a server span per request, child of the W3C trace context the
// caller sent (traceparent), named after the operation ("s3 PutObject")
// with the method, path (never the query), status, client, request id and
// bucket as attributes: no header, no credential. With the tracing
// disabled the middleware is the next handler itself. A missing telemetry
// is a wiring error (domain.ErrInvalidArgument).
func NewTracing(deps TracingDeps, opts ...TracingOption) (Middleware, error) {
	if deps.Telemetry == nil {
		return nil, fmt.Errorf("%w: tracing middleware requires the telemetry", domain.ErrInvalidArgument)
	}
	o := tracingOptions{server: "http"}
	for _, opt := range opts {
		opt(&o)
	}
	return &tracing{deps: deps, opts: o}, nil
}

// Wrap implements Middleware.
func (m *tracing) Wrap(next http.Handler) http.Handler {
	t := m.deps.Telemetry
	if !t.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.opts.only != nil && !m.opts.only(r) {
			next.ServeHTTP(w, r)
			return
		}
		ctx := t.Extract(r.Context(), r.Header)
		ctx, span := t.StartSpan(ctx, m.opts.server+" "+r.Method, trace.SpanOptions{Kind: trace.SpanServer, Attributes: []trace.Attribute{
			{Key: "http.request.method", Value: r.Method},
			{Key: "url.path", Value: r.URL.Path},
			{Key: "server.name", Value: m.opts.server},
			{Key: "client.address", Value: ClientOf(r).IP},
		}})
		defer span.End()
		ctx, holder := observe(ctx)
		r = r.WithContext(ctx)
		rw := wrapWriter(w)
		next.ServeHTTP(rw, r)

		api := holder.Name()
		if api == "" && m.opts.api != nil {
			api = m.opts.api(r)
		}
		if api != "" {
			span.SetName(m.opts.server + " " + api)
		}
		attrs := []trace.Attribute{
			{Key: "http.response.status_code", Value: rw.Status()},
			{Key: "nexo.api", Value: api},
			{Key: "nexo.request_id", Value: requestIDOf(ctx, rw)},
		}
		if m.opts.server == "s3" {
			if bucket, _ := targetOf(r, m.opts.domains); bucket != "" {
				attrs = append(attrs, trace.Attribute{Key: "nexo.bucket", Value: bucket})
			}
		}
		span.SetAttributes(attrs...)
		if rw.Status() >= http.StatusInternalServerError {
			span.SetError(statusError(rw.Status()))
		}
	})
}
