package dist

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

type routeKey struct{}

// WithRoute stores the resolution of the host of a request.
func WithRoute(ctx context.Context, r distribution.Route) context.Context {
	return context.WithValue(ctx, routeKey{}, r)
}

// RouteFromContext returns the resolution stored by the HostRouter.
func RouteFromContext(ctx context.Context) (distribution.Route, bool) {
	r, ok := ctx.Value(routeKey{}).(distribution.Route)
	return r, ok
}

// WebsiteDeps are the dependencies of the website routes.
type WebsiteDeps struct {
	// Website serves the requests (required).
	Website distribution.Website
	// Resolver resolves the host of the requests not routed by the
	// HostRouter (required).
	Resolver distribution.Resolver
	// Verifier authenticates the signed requests (SigV4) of the
	// authenticated behaviors; nil treats them as anonymous.
	Verifier auth.Verifier
	Logger   *slog.Logger
}

// WebsiteOption configures the website routes.
type WebsiteOption func(*websiteRoutes)

// WithSendTimeout bounds how long a download may stall (default: none).
func WithSendTimeout(d time.Duration) WebsiteOption {
	return func(h *websiteRoutes) { h.sendTimeout = d }
}

type websiteRoutes struct {
	deps        WebsiteDeps
	sendTimeout time.Duration
}

var _ WebsiteRoutes = (*websiteRoutes)(nil)

// NewWebsiteRoutes returns the website routes. A missing engine or
// resolver is a wiring error (domain.ErrInvalidArgument).
func NewWebsiteRoutes(deps WebsiteDeps, opts ...WebsiteOption) (WebsiteRoutes, error) {
	if deps.Website == nil || deps.Resolver == nil {
		return nil, fmt.Errorf("%w: website routes need the website engine and the resolver", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	h := &websiteRoutes{deps: deps}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes: the whole URL space, every method.
func (h *websiteRoutes) Register(r chi.Router) {
	r.Handle("/", h)
	r.Handle("/*", h)
}

// isSigned reports a request carrying a SigV4 signature.
func isSigned(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
		return true
	}
	q := r.URL.Query()
	return q.Has("X-Amz-Signature") || q.Has("X-Amz-Algorithm")
}

// identity returns the SigV4 identity of r: the one of the auth
// middleware of the listener, or its own verification of a signed
// request.
func (h *websiteRoutes) identity(r *http.Request) (*domain.Identity, error) {
	if id, ok := middleware.IdentityFromContext(r.Context()); ok && !id.IsAnonymous {
		return id, nil
	}
	if h.deps.Verifier == nil || !isSigned(r) {
		return nil, nil
	}
	ctx := auth.WithExpectedService(r.Context(), auth.ServiceS3)
	return h.deps.Verifier.Verify(ctx, r.WithContext(ctx))
}

// webRequest decodes a request.
func webRequest(r *http.Request, route distribution.Route) distribution.WebRequest {
	client := middleware.ClientOf(r)
	host := client.Host
	if host == "" {
		host = r.Host
	}
	return distribution.WebRequest{
		Route: route, Host: host, Method: r.Method, Path: r.URL.Path, EscapedPath: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery,
		Header: r.Header, ClientIP: client.IP, Secure: client.Secure, Body: r.Body, ContentLength: r.ContentLength,
	}
}

// ServeHTTP implements http.Handler.
func (h *websiteRoutes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	route, ok := RouteFromContext(ctx)
	if !ok {
		route = h.deps.Resolver.Resolve(ctx, middleware.HostOf(r))
	}
	if route.Kind != distribution.RouteDistribution || route.Distribution == nil {
		writePlain(w, r, http.StatusMisdirectedRequest, "no distribution serves this host")
		return
	}
	httpx.SetOperationName(ctx, "Website"+operationOf(r.Method))
	req := webRequest(r, route)
	req.Identity, req.AuthErr = h.identity(r)
	resp, err := h.deps.Website.Serve(ctx, req)
	if err != nil {
		h.deps.Logger.ErrorContext(ctx, "dist: website request failed", "distribution", route.Distribution.ID, "err", err)
		writePlain(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	h.write(ctx, w, r, resp)
}

// write encodes the answer of the engine.
func (h *websiteRoutes) write(ctx context.Context, w http.ResponseWriter, r *http.Request, resp *distribution.WebResponse) {
	if resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	noBody := r.Method == http.MethodHead || resp.Status == http.StatusNotModified || resp.Status == http.StatusNoContent
	if noBody && resp.Status != http.StatusOK && resp.Status != http.StatusPartialContent {
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(resp.Status)
	if noBody || resp.Body == nil {
		return
	}
	if n, err := httpx.CopyBody(w, resp.Body, h.sendTimeout); err != nil {
		h.deps.Logger.DebugContext(ctx, "dist: body interrupted", "key", resp.Key, "written", n, "err", err)
	}
}

// operationOf names the operation of a method for the audit.
func operationOf(method string) string {
	switch method {
	case http.MethodGet:
		return "Get"
	case http.MethodHead:
		return "Head"
	case http.MethodOptions:
		return "Options"
	case http.MethodPut, http.MethodPost:
		return "Upload"
	}
	return "Other"
}

// writePlain writes a plain-text error.
func writePlain(w http.ResponseWriter, r *http.Request, status int, msg string) {
	body := strconv.Itoa(status) + " " + http.StatusText(status) + ": " + msg + "\n"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}
