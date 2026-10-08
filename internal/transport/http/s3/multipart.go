package s3

import (
	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// MultipartDeps are the dependencies of the multipart routes.
type MultipartDeps struct {
	Objects object.Service
	Policy  policy.Service
	Enc     httpx.Encoder
}

type multipartRoutes struct {
	deps MultipartDeps
}

var _ MultipartRoutes = (*multipartRoutes)(nil)

// NewMultipartRoutes returns the multipart route group.
func NewMultipartRoutes(deps MultipartDeps) MultipartRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &multipartRoutes{deps: deps}
}

// MultipartOperations lists the multipart upload operations.
var MultipartOperations = []Operation{
	{Name: "ListMultipartUploads", Method: "GET", Scope: ScopeBucket, Query: "uploads"},
	{Name: "CreateMultipartUpload", Method: "POST", Scope: ScopeObject, Query: "uploads"},
	{Name: "UploadPart", Method: "PUT", Scope: ScopeObject, Query: "uploadId"},
	{Name: "CompleteMultipartUpload", Method: "POST", Scope: ScopeObject, Query: "uploadId"},
	{Name: "AbortMultipartUpload", Method: "DELETE", Scope: ScopeObject, Query: "uploadId"},
	{Name: "ListParts", Method: "GET", Scope: ScopeObject, Query: "uploadId"},
}

// Mount implements Group. UploadPartCopy shares PUT ?uploadId with
// UploadPart and is told apart by x-amz-copy-source in M1.
//
// TODO(M1): implement the handlers.
func (h *multipartRoutes) Mount(d Dispatcher) {
	mountAll(d, h.deps.Enc, MultipartOperations)
}

// Register implements Routes.
func (h *multipartRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }
