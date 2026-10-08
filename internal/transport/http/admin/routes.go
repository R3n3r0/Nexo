// Package admin implements the Admin API transport (/api/v1): JSON in,
// JSON out, problem+json errors. Each functional area is a route group;
// every handler decodes the request, calls a service and encodes the
// response. Authentication is a middleware (console session cookie, see
// middleware.NewSessionAuth); authorization maps every endpoint to an
// admin:* policy action evaluated by the policy engine (docs/04 §4.4).
package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// Routes registers a group of routes on a chi router. Groups register
// paths relative to the API prefix; NewAPI mounts them under Prefix behind
// the authentication middleware.
type Routes = httpx.Routes

// PublicRoutes is implemented by the groups that also expose routes served
// without authentication (the login endpoint).
type PublicRoutes interface {
	Routes
	// RegisterPublic registers the routes reachable without a session.
	RegisterPublic(r chi.Router)
}

// Prefix is the Admin API base path.
const Prefix = "/api/v1"

// SessionRoutes exposes POST /auth/login (public), POST /auth/logout and
// GET /auth/session.
type SessionRoutes interface{ PublicRoutes }

// InfoRoutes exposes GET /info, GET /usage, the healing endpoints and the
// service control endpoint.
type InfoRoutes interface{ Routes }

// UserRoutes exposes /users, /groups and /access-keys.
type UserRoutes interface{ Routes }

// PolicyRoutes exposes /policies and attach/detach.
type PolicyRoutes interface{ Routes }

// BucketRoutes exposes /buckets, the bucket settings and the object browser.
type BucketRoutes interface{ Routes }

// ConfigRoutes exposes /config.
type ConfigRoutes interface{ Routes }

// APIDeps are the dependencies of the API composition.
type APIDeps struct {
	Enc httpx.Encoder
	// Auth authenticates every route except the public ones. Nil leaves
	// the API open (tests).
	Auth middleware.Middleware
}

type api struct {
	deps   APIDeps
	groups []Routes
}

var _ Routes = (*api)(nil)

// NewAPI mounts the groups under Prefix with problem+json 404/405 handlers.
// Public routes (PublicRoutes.RegisterPublic) are served as they are; every
// other route is wrapped by deps.Auth.
func NewAPI(deps APIDeps, groups ...Routes) Routes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &api{deps: deps, groups: groups}
}

// Register implements Routes.
func (a *api) Register(r chi.Router) {
	r.Route(Prefix, func(r chi.Router) {
		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			a.deps.Enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusNotFound, Code: "not_found", Detail: "no such endpoint"})
		})
		r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			a.deps.Enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed"})
		})
		for _, g := range a.groups {
			if p, ok := g.(PublicRoutes); ok {
				p.RegisterPublic(r)
			}
		}
		r.Group(func(r chi.Router) {
			if a.deps.Auth != nil {
				r.Use(a.deps.Auth.Wrap)
			}
			for _, g := range a.groups {
				if g != nil {
					g.Register(r)
				}
			}
		})
	})
}

// notImplemented answers 501 problem+json.
func notImplemented(enc httpx.Encoder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusNotImplemented, Code: "not_implemented", Detail: "endpoint not implemented yet"})
	}
}
