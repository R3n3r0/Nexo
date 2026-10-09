package rpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/metrics"
)

// ServerDeps are the dependencies of the RPC server.
type ServerDeps struct {
	// Auth verifies the internode tokens (required).
	Auth Authenticator
	// Routes are the route groups served below Prefix.
	Routes []Routes
	// Metrics, when set, counts the refused calls
	// (nexo_rpc_auth_refused_total{reason}).
	Metrics metrics.Registry
	Clock   clock.Clock
	Logger  *slog.Logger
}

// pingReply is the answer of the health ping every Conn uses to probe an
// offline node.
type pingReply struct {
	Node     string
	Protocol int
	Time     time.Time
}

type claimsKey struct{}

// CallerFromContext returns the verified claims of the node calling an
// internode route.
func CallerFromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(Claims)
	return c, ok
}

// server implements Server.
type server struct {
	deps    ServerDeps
	opts    serverOptions
	router  http.Handler
	refused metrics.Counter
	// failures limits and logs the authentication failures per remote
	// address.
	failures *failureLimiter
}

var _ Server = (*server)(nil)

// Reasons of a refused call (label of nexo_rpc_auth_refused_total).
const (
	refusedToken      = "invalid_token"
	refusedDeployment = "deployment"
	refusedVersion    = "version"
	refusedUnbound    = "unbound"
	refusedClientCert = "client_certificate"
	refusedThrottled  = "throttled"
)

// NewServer returns the RPC server of the node.
func NewServer(deps ServerDeps, opts ...ServerOption) (Server, error) {
	o := apply(opts)
	if o.failBurst == 0 {
		o.failBurst, o.failRefill = defaultFailBurst, defaultFailRefill
	}
	if deps.Auth == nil {
		return nil, fmt.Errorf("%w: rpc: the server needs the authenticator", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &server{deps: deps, opts: o, failures: newFailureLimiter(o.failBurst, o.failRefill)}
	if deps.Metrics != nil {
		c, err := deps.Metrics.Counter(context.Background(), metrics.Definition{
			Subsystem: "rpc", Name: "auth_refused_total",
			Help:   "Internode calls refused before reaching a route, by reason.",
			Labels: []string{"reason"},
		})
		if err != nil {
			return nil, fmt.Errorf("rpc: metrics: %w", err)
		}
		s.refused = c
	}
	r := chi.NewRouter()
	notFound := func(w http.ResponseWriter, r *http.Request) {
		writeError(w, fmt.Errorf("%w: rpc: no route %s", domain.ErrNotImplemented, r.URL.Path))
	}
	methodNotAllowed := func(w http.ResponseWriter, r *http.Request) {
		writeError(w, fmt.Errorf("%w: rpc: %s %s", domain.ErrMethodNotAllowed, r.Method, r.URL.Path))
	}
	r.NotFound(notFound)
	r.MethodNotAllowed(methodNotAllowed)
	r.Route(Prefix, func(r chi.Router) {
		r.NotFound(notFound)
		r.MethodNotAllowed(methodNotAllowed)
		r.Use(s.authenticate)
		r.Post("/health/ping", unary(s.ping))
		for _, rt := range deps.Routes {
			if rt != nil {
				rt.Register(r)
			}
		}
	})
	s.router = r
	return s, nil
}

// ServeHTTP implements http.Handler. The internode calls are HTTP/2 only:
// an HTTP/1 request below Prefix (a reverse proxy, a replayed request) is
// refused before its token is even looked at.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor < 2 {
		writeError(w, fmt.Errorf("%w: rpc: the internode calls need HTTP/2", domain.ErrNotImplemented))
		return
	}
	s.router.ServeHTTP(w, r)
}

// Wrap implements Server (middleware.Middleware).
func (s *server) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, Prefix+"/") && (s.opts.skip == nil || !s.opts.skip(r)) {
			s.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate verifies the client certificate (mutual TLS) and the
// internode token of every call. A remote address that keeps failing is
// refused without verification for a while; the failures are logged as
// warnings, at most once per address and window.
func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		remote := remoteHost(r.RemoteAddr)
		if !s.failures.allow(remote, s.deps.Clock.Now()) {
			s.refuse(w, r, remote, refusedThrottled, fmt.Errorf("%w: rpc: too many failed authentications from %s", domain.ErrTooManyRequests, remote))
			return
		}
		if s.opts.clientCert && (r.TLS == nil || len(r.TLS.VerifiedChains) == 0) {
			s.fail(w, r, remote, refusedClientCert, fmt.Errorf("%w: rpc: the internode calls need a verified client certificate", domain.ErrUnauthenticated))
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			token = ""
		}
		claims, err := s.deps.Auth.Verify(ctx, token, CallBinding{Method: r.Method, Target: r.URL.RequestURI()})
		if err != nil {
			reason := refusedToken
			switch {
			case errors.Is(err, ErrDeploymentMismatch):
				reason = refusedDeployment
			case errors.Is(err, ErrIncompatibleVersion):
				reason = refusedVersion
			}
			s.fail(w, r, remote, reason, err)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, Prefix)
		switch {
		case claims.Unbound && !bootstrapCall(path):
			s.refuse(w, r, remote, refusedUnbound, fmt.Errorf("%w: rpc: the caller is not bound to a deployment", domain.ErrAccessDenied))
			return
		case claims.Unverified && !bootstrapCall(path) && !formatCall(path):
			s.refuse(w, r, remote, refusedUnbound, fmt.Errorf("%w: rpc: this node is not bound to a deployment yet", domain.ErrAccessDenied))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, claimsKey{}, claims)))
	})
}

// fail refuses a call whose authentication failed: the failure counts
// against the remote address.
func (s *server) fail(w http.ResponseWriter, r *http.Request, remote, reason string, err error) {
	s.failures.failed(remote, s.deps.Clock.Now())
	s.refuse(w, r, remote, reason, err)
}

// refuse answers err to a call refused for reason, counts it and logs it
// as a warning (with the remote address and the path, never the token),
// at most once per remote address and reason per window.
func (s *server) refuse(w http.ResponseWriter, r *http.Request, remote, reason string, err error) {
	ctx := r.Context()
	if s.refused != nil {
		s.refused.Add(ctx, 1, reason)
	}
	if suppressed, ok := s.failures.shouldLog(remote+" "+reason, s.deps.Clock.Now()); ok {
		s.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "rpc: internode call refused",
			slog.String("reason", reason), slog.String("remote", r.RemoteAddr), slog.String("path", r.URL.Path),
			slog.Int("suppressed", suppressed), slog.Any("err", err))
	}
	writeError(w, err)
}

// bootstrapCall reports the calls a node may make before it knows its
// deployment (Claims.Unbound), and the calls a node that does not know its
// own deployment accepts (Claims.Unverified): the health ping, the
// bootstrap exchange and the read-only calls of the bootstrap on the
// drives (state of a drive; of the files, only format.json is read or
// stat'ed: the storage routes check it). Nothing that writes, lists or
// reads data.
func bootstrapCall(path string) bool {
	switch path {
	case "/health/ping", "/peer/ping", "/storage/disk-info", "/storage/healing", "/storage/read-all", "/storage/stat-info-file":
		return true
	}
	return strings.HasPrefix(path, "/bootstrap/")
}

// formatCall reports the writes a node that does not know its deployment
// yet accepts (Claims.Unverified): the node formatting the deployment
// writes format.json, and the healing tracker of a replaced drive, on the
// drives of the others (the storage routes check the file).
func formatCall(path string) bool { return path == "/storage/write-all" }

// ping answers the health probe of a Conn.
func (s *server) ping(context.Context, struct{}) (pingReply, error) {
	return pingReply{Node: s.deps.Auth.NodeID(), Protocol: ProtocolVersion, Time: s.deps.Clock.Now()}, nil
}
