package s3

import (
	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// PresignedDeps are the dependencies of the presigned routes.
type PresignedDeps struct {
	Verifier auth.Verifier
	Enc      httpx.Encoder
}

type presignedRoutes struct {
	deps PresignedDeps
}

var _ PresignedRoutes = (*presignedRoutes)(nil)

// NewPresignedRoutes returns the presigned route group.
func NewPresignedRoutes(deps PresignedDeps) PresignedRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &presignedRoutes{deps: deps}
}

// PresignedOperations lists the operations reachable through a presigned
// URL. They are discriminated by the X-Amz-Signature query parameter and
// take precedence over the plain object operations.
var PresignedOperations = []Operation{
	{Name: "PresignedGetObject", Method: "GET", Scope: ScopeObject, Query: "X-Amz-Signature"},
	{Name: "PresignedHeadObject", Method: "HEAD", Scope: ScopeObject, Query: "X-Amz-Signature"},
	{Name: "PresignedPutObject", Method: "PUT", Scope: ScopeObject, Query: "X-Amz-Signature"},
	{Name: "PresignedDeleteObject", Method: "DELETE", Scope: ScopeObject, Query: "X-Amz-Signature"},
	{Name: "PresignedListObjects", Method: "GET", Scope: ScopeBucket, Query: "X-Amz-Signature"},
}

// Mount implements Group.
//
// TODO(M2): verify the presigned signature then delegate to the object handlers.
func (h *presignedRoutes) Mount(d Dispatcher) {
	mountAll(d, h.deps.Enc, PresignedOperations)
}

// Register implements Routes.
func (h *presignedRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }
