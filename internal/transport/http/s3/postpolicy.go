package s3

import (
	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// PostPolicyDeps are the dependencies of the POST policy routes.
type PostPolicyDeps struct {
	Verifier auth.Verifier
	Objects  object.Service
	Enc      httpx.Encoder
}

type postPolicyRoutes struct {
	deps PostPolicyDeps
}

var _ PostPolicyRoutes = (*postPolicyRoutes)(nil)

// NewPostPolicyRoutes returns the POST policy route group.
func NewPostPolicyRoutes(deps PostPolicyDeps) PostPolicyRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &postPolicyRoutes{deps: deps}
}

// PostPolicyOperations lists the browser upload operation: POST on the
// bucket without a discriminating query (DeleteObjects uses ?delete).
var PostPolicyOperations = []Operation{
	{Name: "PostObject", Method: "POST", Scope: ScopeBucket},
}

// Mount implements Group.
//
// TODO(M2): parse the multipart form, verify the policy and store the object.
func (h *postPolicyRoutes) Mount(d Dispatcher) {
	mountAll(d, h.deps.Enc, PostPolicyOperations)
}

// Register implements Routes.
func (h *postPolicyRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }
