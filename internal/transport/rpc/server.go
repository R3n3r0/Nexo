package rpc

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// ServerDeps are the dependencies of the RPC server.
type ServerDeps struct {
	// Auth verifies the internode tokens (required).
	Auth Authenticator
	// Routes are the route groups served below Prefix.
	Routes []Routes
	Clock  clock.Clock
	Logger *slog.Logger
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
	deps   ServerDeps
	router http.Handler
}

var _ Server = (*server)(nil)

// NewServer returns the RPC server of the node.
func NewServer(deps ServerDeps) (Server, error) {
	if deps.Auth == nil {
		return nil, fmt.Errorf("%w: rpc: the server needs the authenticator", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &server{deps: deps}
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

// ServeHTTP implements http.Handler.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.router.ServeHTTP(w, r) }

// Wrap implements Server (middleware.Middleware).
func (s *server) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, Prefix+"/") {
			s.router.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate verifies the internode token of every call.
func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			token = ""
		}
		claims, err := s.deps.Auth.Verify(r.Context(), token)
		if err != nil {
			s.deps.Logger.LogAttrs(r.Context(), slog.LevelDebug, "rpc: call refused",
				slog.String("path", r.URL.Path), slog.String("remote", r.RemoteAddr), slog.Any("err", err))
			writeError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey{}, claims)))
	})
}

// ping answers the health probe of a Conn.
func (s *server) ping(context.Context, struct{}) (pingReply, error) {
	return pingReply{Node: s.deps.Auth.NodeID(), Protocol: ProtocolVersion, Time: s.deps.Clock.Now()}, nil
}
