package s3

import (
	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// ObjectDeps are the dependencies of the object routes.
type ObjectDeps struct {
	Objects object.Service
	Policy  policy.Service
	Enc     httpx.Encoder
}

type objectRoutes struct {
	deps ObjectDeps
}

var _ ObjectRoutes = (*objectRoutes)(nil)

// NewObjectRoutes returns the object route group.
func NewObjectRoutes(deps ObjectDeps) ObjectRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &objectRoutes{deps: deps}
}

// ObjectOperations lists the object-level S3 operations.
var ObjectOperations = []Operation{
	{Name: "PutObject", Method: "PUT", Scope: ScopeObject},
	{Name: "GetObject", Method: "GET", Scope: ScopeObject},
	{Name: "HeadObject", Method: "HEAD", Scope: ScopeObject},
	{Name: "DeleteObject", Method: "DELETE", Scope: ScopeObject},
	{Name: "DeleteObjects", Method: "POST", Scope: ScopeBucket, Query: "delete"},
	{Name: "GetObjectTagging", Method: "GET", Scope: ScopeObject, Query: "tagging"},
	{Name: "PutObjectTagging", Method: "PUT", Scope: ScopeObject, Query: "tagging"},
	{Name: "DeleteObjectTagging", Method: "DELETE", Scope: ScopeObject, Query: "tagging"},
	{Name: "GetObjectRetention", Method: "GET", Scope: ScopeObject, Query: "retention"},
	{Name: "PutObjectRetention", Method: "PUT", Scope: ScopeObject, Query: "retention"},
	{Name: "GetObjectLegalHold", Method: "GET", Scope: ScopeObject, Query: "legal-hold"},
	{Name: "PutObjectLegalHold", Method: "PUT", Scope: ScopeObject, Query: "legal-hold"},
	{Name: "GetObjectAcl", Method: "GET", Scope: ScopeObject, Query: "acl"},
	{Name: "PutObjectAcl", Method: "PUT", Scope: ScopeObject, Query: "acl"},
	{Name: "GetObjectAttributes", Method: "GET", Scope: ScopeObject, Query: "attributes"},
}

// Mount implements Group. CopyObject shares PUT /{bucket}/{key} with
// PutObject and is told apart by the x-amz-copy-source header in M1.
//
// TODO(M1): implement the handlers.
func (h *objectRoutes) Mount(d Dispatcher) {
	mountAll(d, h.deps.Enc, ObjectOperations)
}

// Register implements Routes.
func (h *objectRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }
