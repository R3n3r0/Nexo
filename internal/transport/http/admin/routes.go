// Package admin implements the Admin API transport (/api/v1): JSON in,
// JSON out, problem+json errors (RFC 9457) with a stable snake_case code.
// Each functional area is a route group; every handler decodes the
// request, calls a service and encodes the response.
//
// Authentication is a middleware (NewAuthenticator): the console session
// cookie with its CSRF token, or a SigV4 signature for the CLI and the
// SDKs. Authorization is declared by every route (a requirement listing
// its admin:* or s3:* actions) and enforced by a guard that asks the
// authorization service of internal/service/admin, which evaluates the
// actions with the policy engine (docs/04 §4.4); the operations whose
// resources travel in the body are authorized object by object by their
// service.
package admin

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
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

// SessionRoutes exposes POST /auth/login (public), POST /auth/logout,
// GET /auth/session and GET /me/permissions.
type SessionRoutes interface{ PublicRoutes }

// InfoRoutes exposes GET /info, GET /usage, the healing endpoints, the
// service control endpoints and the live streams.
type InfoRoutes interface{ Routes }

// UserRoutes exposes /users, /groups and /access-keys.
type UserRoutes interface{ Routes }

// PolicyRoutes exposes /policies: CRUD, attach/detach, entities,
// simulation and validation.
type PolicyRoutes interface{ Routes }

// BucketRoutes exposes /buckets and the bucket settings (versioning,
// policy, quota, tags, encryption, object lock, anonymous access).
type BucketRoutes interface{ Routes }

// ObjectRoutes exposes the object browser of the console: listing, stat,
// versions, folders, deletion, rename, tags, proxied uploads (single and
// multipart), downloads, previews and zip archives.
type ObjectRoutes interface{ Routes }

// ShareRoutes exposes the managed share links (/shares), the raw
// presigned URLs (/presign) and the POST policy forms (/post-policy).
type ShareRoutes interface{ Routes }

// ConfigRoutes exposes /config.
type ConfigRoutes interface{ Routes }

// ClusterRoutes exposes the export and import of the state of the
// deployment (/cluster/export, /cluster/import) and the diagnostics of the
// namespace locks (/locks).
type ClusterRoutes interface{ Routes }

// APIDeps are the dependencies of the API composition.
type APIDeps struct {
	Enc httpx.Encoder
	// Auth authenticates every route except the public ones
	// (NewAuthenticator; required).
	Auth middleware.Middleware
}

type api struct {
	deps   APIDeps
	groups []Routes
}

var _ Routes = (*api)(nil)

// NewAPI mounts the groups under Prefix with problem+json 404/405 handlers.
// Public routes (PublicRoutes.RegisterPublic) are served as they are; every
// other route is wrapped by deps.Auth. A missing authentication middleware
// or a nil group is a wiring error (domain.ErrInvalidArgument): the API is
// never served unauthenticated.
func NewAPI(deps APIDeps, groups ...Routes) (Routes, error) {
	if deps.Auth == nil {
		return nil, fmt.Errorf("%w: the Admin API requires the authentication middleware", domain.ErrInvalidArgument)
	}
	for i, g := range groups {
		if g == nil {
			return nil, fmt.Errorf("%w: Admin API route group %d is nil", domain.ErrInvalidArgument, i)
		}
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &api{deps: deps, groups: groups}, nil
}

// Register implements Routes.
func (a *api) Register(r chi.Router) {
	r.Route(Prefix, func(r chi.Router) {
		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			problem(a.deps.Enc, w, r, http.StatusNotFound, "not_found", "no such endpoint")
		})
		r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			problem(a.deps.Enc, w, r, http.StatusMethodNotAllowed, "method_not_allowed", "")
		})
		for _, g := range a.groups {
			if p, ok := g.(PublicRoutes); ok {
				p.RegisterPublic(r)
			}
		}
		r.Group(func(r chi.Router) {
			r.Use(a.deps.Auth.Wrap)
			for _, g := range a.groups {
				g.Register(r)
			}
		})
	})
}
