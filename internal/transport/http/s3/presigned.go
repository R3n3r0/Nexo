package s3

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// presignedQuery is the query parameter that marks a presigned request.
const presignedQuery = "X-Amz-Signature"

// PresignedDeps are the dependencies of the presigned routes. Verifier is
// used only when no auth middleware established an identity (tests,
// standalone registration): the S3 server verifies presigned signatures in
// the auth middleware. Objects, Buckets and Policy are required.
type PresignedDeps struct {
	Verifier auth.Verifier
	Objects  object.Service
	Buckets  bucket.Service
	Policy   policy.Service
	// Media serves the variants of the images (ObjectDeps.Media).
	Media  media.Service
	Enc    httpx.Codec
	Clock  clock.Clock
	Logger *slog.Logger
}

type presignedRoutes struct {
	deps    PresignedDeps
	objects *objectRoutes
	buckets *bucketRoutes
}

var _ PresignedRoutes = (*presignedRoutes)(nil)

// NewPresignedRoutes returns the presigned route group: the object and
// listing operations, reached through a signed query string. The
// permissions are those of the signer, evaluated when the URL is used.
func NewPresignedRoutes(deps PresignedDeps, opts ...Option) PresignedRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &presignedRoutes{
		deps: deps,
		objects: newObjectRoutes(ObjectDeps{
			Objects: deps.Objects, Policy: deps.Policy, Media: deps.Media, Enc: deps.Enc, Clock: deps.Clock, Logger: deps.Logger,
		}, opts...),
		buckets: newBucketRoutes(BucketDeps{
			Buckets: deps.Buckets, Objects: deps.Objects, Policy: deps.Policy, Enc: deps.Enc, Clock: deps.Clock, Logger: deps.Logger,
		}, opts...),
	}
}

// operations lists the operations reachable through a presigned URL. They
// are discriminated by the X-Amz-Signature query parameter, which ranks
// below the sub-resources: a presigned UploadPart (?uploadId) or
// GetObjectTagging (?tagging) reaches its own operation.
func (h *presignedRoutes) operations() map[Operation]http.HandlerFunc {
	return map[Operation]http.HandlerFunc{
		{Name: "PresignedGetObject", Method: http.MethodGet, Scope: ScopeObject, Query: presignedQuery}:       h.objects.getObject,
		{Name: "PresignedHeadObject", Method: http.MethodHead, Scope: ScopeObject, Query: presignedQuery}:     h.objects.headObject,
		{Name: "PresignedPutObject", Method: http.MethodPut, Scope: ScopeObject, Query: presignedQuery}:       h.objects.putObject,
		{Name: "PresignedDeleteObject", Method: http.MethodDelete, Scope: ScopeObject, Query: presignedQuery}: h.objects.deleteObject,
		{Name: "PresignedListObjects", Method: http.MethodGet, Scope: ScopeBucket, Query: presignedQuery}:     h.listObjects,
	}
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
	mountAll(d, h.deps.Enc, ops, h.objects.available())
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
