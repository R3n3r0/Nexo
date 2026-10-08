// Package admin implements the Admin API transport (/api/v1): JSON in,
// JSON out, problem+json errors. Each functional area is a route group.
package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// Routes registers a group of routes on a chi router. Groups register
// paths relative to the API prefix; NewAPI mounts them under /api/v1.
type Routes = httpx.Routes

// Prefix is the Admin API base path.
const Prefix = "/api/v1"

// SessionRoutes exposes POST /auth/login, POST /auth/logout and GET /auth/session.
type SessionRoutes interface{ Routes }

// InfoRoutes exposes GET /info and GET /usage.
type InfoRoutes interface{ Routes }

// UserRoutes exposes /users, /groups and /access-keys.
type UserRoutes interface{ Routes }

// PolicyRoutes exposes /policies and attach/detach.
type PolicyRoutes interface{ Routes }

// BucketRoutes exposes /buckets and the bucket settings.
type BucketRoutes interface{ Routes }

// ConfigRoutes exposes /config.
type ConfigRoutes interface{ Routes }

type api struct {
	groups []Routes
	enc    httpx.Encoder
}

var _ Routes = (*api)(nil)

// NewAPI mounts the groups under Prefix with problem+json 404/405 handlers.
func NewAPI(enc httpx.Encoder, groups ...Routes) Routes {
	if enc == nil {
		enc = httpx.New()
	}
	return &api{groups: groups, enc: enc}
}

// Register implements Routes.
func (a *api) Register(r chi.Router) {
	r.Route(Prefix, func(r chi.Router) {
		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			a.enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusNotFound, Code: "not_found", Detail: "no such endpoint"})
		})
		r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			a.enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed"})
		})
		for _, g := range a.groups {
			if g != nil {
				g.Register(r)
			}
		}
	})
}

// notImplemented answers 501 problem+json.
func notImplemented(enc httpx.Encoder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusNotImplemented, Code: "not_implemented", Detail: "endpoint not implemented yet"})
	}
}
