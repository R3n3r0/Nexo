package s3

import (
	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

type chiRouter = chi.Router

// BucketDeps are the dependencies of the bucket routes.
type BucketDeps struct {
	// Buckets and Objects are not used yet (TODO M1); they may be nil.
	Buckets bucket.Service
	Objects object.Service
	Policy  policy.Service
	Enc     httpx.Encoder
}

type bucketRoutes struct {
	deps BucketDeps
}

var _ BucketRoutes = (*bucketRoutes)(nil)

// NewBucketRoutes returns the bucket route group.
func NewBucketRoutes(deps BucketDeps) BucketRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &bucketRoutes{deps: deps}
}

// BucketOperations lists the bucket-level S3 operations.
var BucketOperations = []Operation{
	{Name: "ListBuckets", Method: "GET", Scope: ScopeService},
	{Name: "CreateBucket", Method: "PUT", Scope: ScopeBucket},
	{Name: "HeadBucket", Method: "HEAD", Scope: ScopeBucket},
	{Name: "DeleteBucket", Method: "DELETE", Scope: ScopeBucket},
	{Name: "ListObjects", Method: "GET", Scope: ScopeBucket},
	{Name: "ListObjectsV2", Method: "GET", Scope: ScopeBucket, Query: "list-type", QueryValue: "2"},
	{Name: "ListObjectVersions", Method: "GET", Scope: ScopeBucket, Query: "versions"},
	{Name: "GetBucketLocation", Method: "GET", Scope: ScopeBucket, Query: "location"},
	{Name: "GetBucketPolicy", Method: "GET", Scope: ScopeBucket, Query: "policy"},
	{Name: "PutBucketPolicy", Method: "PUT", Scope: ScopeBucket, Query: "policy"},
	{Name: "DeleteBucketPolicy", Method: "DELETE", Scope: ScopeBucket, Query: "policy"},
	{Name: "GetBucketPolicyStatus", Method: "GET", Scope: ScopeBucket, Query: "policyStatus"},
	{Name: "GetBucketVersioning", Method: "GET", Scope: ScopeBucket, Query: "versioning"},
	{Name: "PutBucketVersioning", Method: "PUT", Scope: ScopeBucket, Query: "versioning"},
	{Name: "GetBucketTagging", Method: "GET", Scope: ScopeBucket, Query: "tagging"},
	{Name: "PutBucketTagging", Method: "PUT", Scope: ScopeBucket, Query: "tagging"},
	{Name: "DeleteBucketTagging", Method: "DELETE", Scope: ScopeBucket, Query: "tagging"},
	{Name: "GetBucketCors", Method: "GET", Scope: ScopeBucket, Query: "cors"},
	{Name: "PutBucketCors", Method: "PUT", Scope: ScopeBucket, Query: "cors"},
	{Name: "DeleteBucketCors", Method: "DELETE", Scope: ScopeBucket, Query: "cors"},
	{Name: "GetBucketLifecycle", Method: "GET", Scope: ScopeBucket, Query: "lifecycle"},
	{Name: "PutBucketLifecycle", Method: "PUT", Scope: ScopeBucket, Query: "lifecycle"},
	{Name: "DeleteBucketLifecycle", Method: "DELETE", Scope: ScopeBucket, Query: "lifecycle"},
	{Name: "GetObjectLockConfiguration", Method: "GET", Scope: ScopeBucket, Query: "object-lock"},
	{Name: "PutObjectLockConfiguration", Method: "PUT", Scope: ScopeBucket, Query: "object-lock"},
	{Name: "GetBucketEncryption", Method: "GET", Scope: ScopeBucket, Query: "encryption"},
	{Name: "PutBucketEncryption", Method: "PUT", Scope: ScopeBucket, Query: "encryption"},
	{Name: "DeleteBucketEncryption", Method: "DELETE", Scope: ScopeBucket, Query: "encryption"},
	{Name: "GetBucketNotification", Method: "GET", Scope: ScopeBucket, Query: "notification"},
	{Name: "PutBucketNotification", Method: "PUT", Scope: ScopeBucket, Query: "notification"},
	{Name: "GetBucketAcl", Method: "GET", Scope: ScopeBucket, Query: "acl"},
	{Name: "PutBucketAcl", Method: "PUT", Scope: ScopeBucket, Query: "acl"},
}

// Mount implements Group.
//
// TODO(M1): replace the NotImplemented handlers with decode → service → encode.
func (h *bucketRoutes) Mount(d Dispatcher) {
	mountAll(d, h.deps.Enc, BucketOperations)
}

// Register implements Routes.
func (h *bucketRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }
