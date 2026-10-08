package s3

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// PresignedDeps are the dependencies of the presigned routes. Verifier is
// used only when no auth middleware established an identity (tests,
// standalone registration): the S3 server verifies presigned signatures in
// the auth middleware.
type PresignedDeps struct {
	Verifier auth.Verifier
	Objects  object.Service
	Buckets  bucket.Service
	Policy   policy.Service
	Enc      httpx.Codec
}

type presignedRoutes struct {
	deps    PresignedDeps
	objects *objectRoutes
	buckets *bucketRoutes
}

var _ PresignedRoutes = (*presignedRoutes)(nil)

// NewPresignedRoutes returns the presigned route group: the object and
// listing operations, reached through a signed query string.
func NewPresignedRoutes(deps PresignedDeps) PresignedRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	objects, _ := NewObjectRoutes(ObjectDeps{Objects: deps.Objects, Buckets: deps.Buckets, Policy: deps.Policy, Enc: deps.Enc}).(*objectRoutes)
	buckets, _ := NewBucketRoutes(BucketDeps{Buckets: deps.Buckets, Objects: deps.Objects, Policy: deps.Policy, Enc: deps.Enc}).(*bucketRoutes)
	return &presignedRoutes{deps: deps, objects: objects, buckets: buckets}
}

// presignedOperations lists the operations reachable through a presigned
// URL. They are discriminated by the X-Amz-Signature query parameter and
// take precedence over the plain object operations.
func (h *presignedRoutes) operations() map[Operation]http.HandlerFunc {
	ops := map[Operation]http.HandlerFunc{
		{Name: "PresignedGetObject", Method: "GET", Scope: ScopeObject, Query: "X-Amz-Signature"}:       h.objects.getObject,
		{Name: "PresignedHeadObject", Method: "HEAD", Scope: ScopeObject, Query: "X-Amz-Signature"}:     h.objects.headObject,
		{Name: "PresignedPutObject", Method: "PUT", Scope: ScopeObject, Query: "X-Amz-Signature"}:       h.objects.putObject,
		{Name: "PresignedDeleteObject", Method: "DELETE", Scope: ScopeObject, Query: "X-Amz-Signature"}: h.objects.deleteObject,
		{Name: "PresignedListObjects", Method: "GET", Scope: ScopeBucket, Query: "X-Amz-Signature"}:     h.listObjects,
	}
	return ops
}

// listObjects picks the v1 or v2 listing from the query.
func (h *presignedRoutes) listObjects(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("list-type") == "2" {
		h.buckets.listObjectsV2(w, r)
		return
	}
	h.buckets.listObjects(w, r)
}

// Mount implements Group.
func (h *presignedRoutes) Mount(d Dispatcher) {
	ops := make(map[Operation]http.HandlerFunc, 5)
	for op, fn := range h.operations() {
		ops[op] = h.verified(fn)
	}
	mountAll(d, h.deps.Enc, ops, h.deps.Objects != nil && h.deps.Buckets != nil)
}

// verified authenticates the presigned request when no middleware did.
func (h *presignedRoutes) verified(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := middleware.IdentityFromContext(r.Context()); !ok && h.deps.Verifier != nil {
			id, err := h.deps.Verifier.VerifyPresigned(r.Context(), r)
			if err != nil {
				h.deps.Enc.WriteS3Error(w, r, err)
				return
			}
			r = r.WithContext(middleware.WithIdentity(r.Context(), id))
		}
		next(w, r)
	}
}

// Register implements Routes.
func (h *presignedRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }
