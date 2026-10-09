package dist

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// HostRouterDeps are the dependencies of the host router.
type HostRouterDeps struct {
	// Resolver resolves the hosts (required).
	Resolver distribution.Resolver
	// Website serves the website distributions (required).
	Website WebsiteRoutes
	// Admission admits the requests of the s3 distributions (required).
	Admission distribution.Website
	// Registry receives the metrics of the distributions (optional).
	Registry metrics.Registry
	// AccessLog writes the access log of the distributions that enable
	// it (optional).
	AccessLog AccessLogger
	// Cache serves the requests of the distributions from the cache of
	// docs/11 (optional): every request of a distribution begins an
	// exchange with it, whose context the object reads of the request
	// carry, and whose headers (X-Cache, Age, Vary...) the answer gets.
	Cache  cache.Runtime
	Clock  clock.Clock
	Logger *slog.Logger
}

// HostRouterOption configures the host router.
type HostRouterOption func(*hostRouter)

// WithDistributionsOnly answers 421 to every host that is not an alias of
// a distribution: the web listeners serve the distributions only.
func WithDistributionsOnly() HostRouterOption {
	return func(h *hostRouter) { h.distributionsOnly = true }
}

// WithPassthrough leaves the requests whose path starts with one of the
// prefixes to the routes of the listener, whatever their host (the ACME
// challenges of the web listener).
func WithPassthrough(prefixes ...string) HostRouterOption {
	return func(h *hostRouter) { h.passthrough = append(h.passthrough, prefixes...) }
}

// WithListenerName labels the metrics of the listener ("s3", "web").
func WithListenerName(name string) HostRouterOption {
	return func(h *hostRouter) { h.listener = name }
}

type hostRouter struct {
	deps              HostRouterDeps
	distributionsOnly bool
	passthrough       []string
	listener          string

	requests metrics.Counter
	bytes    metrics.Counter
	duration metrics.Histogram
}

var _ HostRouter = (*hostRouter)(nil)

// NewHostRouter returns the host router. A missing resolver, website
// routes or admission is a wiring error (domain.ErrInvalidArgument).
func NewHostRouter(deps HostRouterDeps, opts ...HostRouterOption) (HostRouter, error) {
	if deps.Resolver == nil || deps.Website == nil || deps.Admission == nil {
		return nil, fmt.Errorf("%w: the host router needs the resolver, the website routes and the admission", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	h := &hostRouter{deps: deps, listener: "http"}
	for _, o := range opts {
		o(h)
	}
	if deps.Registry != nil {
		ctx := context.Background()
		var err error
		if h.requests, err = deps.Registry.Counter(ctx, metrics.Definition{
			Subsystem: "distribution", Name: "requests_total",
			Help:   "Requests served by the distributions, by distribution, alias and status.",
			Labels: []string{"distribution", "host", "status"},
		}); err != nil {
			return nil, fmt.Errorf("host router metrics: %w", err)
		}
		if h.bytes, err = deps.Registry.Counter(ctx, metrics.Definition{
			Subsystem: "distribution", Name: "bytes_served_total",
			Help:   "Bytes of the answers of the distributions, by distribution and alias.",
			Labels: []string{"distribution", "host"},
		}); err != nil {
			return nil, fmt.Errorf("host router metrics: %w", err)
		}
		if h.duration, err = deps.Registry.Histogram(ctx, metrics.Definition{
			Subsystem: "distribution", Name: "request_duration_seconds",
			Help:   "Latency of the requests of the distributions.",
			Labels: []string{"distribution"},
		}); err != nil {
			return nil, fmt.Errorf("host router metrics: %w", err)
		}
	}
	return h, nil
}

// Wrap implements middleware.Middleware.
func (h *hostRouter) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range h.passthrough {
			if strings.HasPrefix(r.URL.Path, p) {
				next.ServeHTTP(w, r)
				return
			}
		}
		ctx := r.Context()
		route := h.deps.Resolver.Resolve(ctx, middleware.HostOf(r))
		switch {
		case route.Kind == distribution.RouteDistribution && route.Distribution != nil:
			h.serveDistribution(w, r.WithContext(WithRoute(ctx, route)), route, next)
		case route.Kind == distribution.RouteMisdirected || h.distributionsOnly:
			writePlain(w, r, http.StatusMisdirectedRequest, "no distribution serves this host")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// serveDistribution serves a request of a distribution and records it.
func (h *hostRouter) serveDistribution(w http.ResponseWriter, r *http.Request, route distribution.Route, next http.Handler) {
	start := h.deps.Clock.Now()
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	if h.deps.Cache != nil {
		ctx, ex := h.deps.Cache.Begin(r.Context(), cacheRequest(r, route))
		r = r.WithContext(ctx)
		rec.decorate = ex.Decorate
		defer func() { ex.Finish(r.Context(), rec.status, rec.bytes) }()
	}
	d := route.Distribution
	if d.Mode == distribution.ModeS3 {
		if resp := h.deps.Admission.Admit(r.Context(), webRequest(r, route)); resp != nil {
			writeResponse(rec, r, resp)
		} else {
			next.ServeHTTP(rec, r)
		}
	} else {
		h.deps.Website.ServeHTTP(rec, r)
	}
	h.record(r, route, rec, start)
}

// record updates the metrics and the access log of a distribution.
func (h *hostRouter) record(r *http.Request, route distribution.Route, rec *recorder, start time.Time) {
	ctx := r.Context()
	d := route.Distribution
	elapsed := h.deps.Clock.Now().Sub(start)
	if h.requests != nil {
		h.requests.Add(ctx, 1, d.ID, route.Alias, strconv.Itoa(rec.status))
		h.bytes.Add(ctx, float64(rec.bytes), d.ID, route.Alias)
		h.duration.Observe(ctx, elapsed.Seconds(), d.ID)
	}
	if !d.Logging.AccessLog || h.deps.AccessLog == nil {
		return
	}
	client := middleware.ClientOf(r)
	format := d.Logging.Format
	if format == "" {
		format = distribution.LogFormatJSON
	}
	h.deps.AccessLog.Log(ctx, AccessEntry{
		Time: start, Distribution: d.ID, Format: format, Host: middleware.HostOf(r), Alias: route.Alias,
		Method: r.Method, URI: r.URL.RequestURI(), Protocol: r.Proto, Status: rec.status, Bytes: rec.bytes, Duration: elapsed,
		ClientIP: client.IP, UserAgent: r.UserAgent(), Referer: r.Referer(), RequestID: httpx.RequestIDFrom(ctx),
	})
}

// writeResponse writes an answer of the engine (admission refusals).
func writeResponse(w http.ResponseWriter, r *http.Request, resp *distribution.WebResponse) {
	if resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.Status)
	if resp.Body != nil && r.Method != http.MethodHead {
		_, _ = httpx.CopyBody(w, resp.Body, 0)
	}
}

// recorder records the status and the size of an answer. It forwards
// Flush and exposes the wrapped writer (http.ResponseController);
// decorate adds the headers of the cache to the answer.
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
	decorate    func(status int, h http.Header)
}

func (w *recorder) WriteHeader(status int) {
	if !w.wroteHeader {
		w.status, w.wroteHeader = status, true
		if w.decorate != nil && status >= http.StatusOK {
			w.decorate(status, w.Header())
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush implements http.Flusher.
func (w *recorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// cacheRequest decodes a request of a distribution for the cache.
func cacheRequest(r *http.Request, route distribution.Route) cache.Request {
	client := middleware.ClientOf(r)
	req := cache.Request{
		Distribution: route.Distribution, Alias: route.Alias, Host: route.Host, Method: r.Method, Path: r.URL.Path,
		RawQuery: r.URL.RawQuery, Header: r.Header, Secure: client.Secure, ClientIP: client.IP,
		Credentials: carriesCredentials(r),
	}
	if id, ok := middleware.IdentityFromContext(r.Context()); ok && id != nil && !id.IsAnonymous {
		req.Credentials, req.Credential = true, id.AccessKey
	}
	return req
}

// carriesCredentials reports a request with an Authorization header or a
// presigned query (S3 signature versions 2 and 4). The signed URLs of the
// distributions are not credentials: they are verified before the cache
// is looked up.
func carriesCredentials(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return true
	}
	q := r.URL.Query()
	return q.Has("X-Amz-Signature") || q.Has("X-Amz-Credential") || q.Has("AWSAccessKeyId")
}
