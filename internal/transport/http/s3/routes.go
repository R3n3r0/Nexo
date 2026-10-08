// Package s3 implements the S3 API transport. S3 selects the operation from
// the HTTP method, the URL scope (service, bucket or object) and a
// discriminating query parameter (?uploads, ?tagging, ...), which a plain
// path router cannot express. The Dispatcher owns the path-style patterns
// ("/", "/{bucket}", "/{bucket}/*") and selects the operation; the route
// groups (BucketRoutes, ObjectRoutes, ...) mount their operations on it.
//
// Every group is also a Routes: registering it alone on a chi router mounts
// it on a private dispatcher, which is handy for tests.
package s3

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// Routes registers a group of routes on a chi router.
type Routes = httpx.Routes

// Scope is the URL scope of an S3 operation.
type Scope int

// URL scopes.
const (
	// ScopeService is "/" (ListBuckets).
	ScopeService Scope = iota
	// ScopeBucket is "/{bucket}".
	ScopeBucket
	// ScopeObject is "/{bucket}/{key...}".
	ScopeObject
)

// Operation identifies an S3 API operation.
type Operation struct {
	// Name is the S3 operation name (e.g. "ListObjectsV2"), used in logs,
	// audit and policy mapping.
	Name string
	// Method is the HTTP method.
	Method string
	Scope  Scope
	// Query is the discriminating query parameter that must be present
	// (e.g. "uploads"); empty for the default operation of a method/scope.
	Query string
	// QueryValue, when set, must equal the Query parameter's value
	// (e.g. list-type=2).
	QueryValue string
}

// Dispatcher routes S3 requests to operations.
type Dispatcher interface {
	Routes
	// Handle registers the handler of an operation. Registering the same
	// operation twice panics: it is a wiring error.
	Handle(op Operation, h http.HandlerFunc)
}

// Group mounts a family of S3 operations on a Dispatcher.
type Group interface {
	Mount(d Dispatcher)
}

// BucketRoutes exposes the bucket operations: ListBuckets, Create/Head/
// Delete bucket, ListObjects (v1, v2, versions) and the bucket sub-resources.
type BucketRoutes interface {
	Routes
	Group
}

// ObjectRoutes exposes Put/Get/Head/Delete/Copy object, DeleteObjects and
// object tagging.
type ObjectRoutes interface {
	Routes
	Group
}

// MultipartRoutes exposes the multipart upload operations.
type MultipartRoutes interface {
	Routes
	Group
}

// PresignedRoutes handles requests authenticated by a presigned query
// string (X-Amz-Signature). It is mounted as a Group so that it can refuse
// operations that must not be presigned.
type PresignedRoutes interface {
	Routes
	Group
}

// PostPolicyRoutes exposes the browser POST upload (multipart/form-data).
type PostPolicyRoutes interface {
	Routes
	Group
}

// HealthRoutes exposes /nexo/health/{live,ready,cluster}.
type HealthRoutes interface{ Routes }

// MetricsRoutes exposes /nexo/metrics/v1/{cluster,node,bucket}.
type MetricsRoutes interface{ Routes }

// BucketName returns the bucket of the request from the route parameters.
func BucketName(r *http.Request) string {
	return chi.URLParam(r, "bucket")
}

// ObjectKey returns the (decoded) object key of the request: the path after
// "/{bucket}/".
func ObjectKey(r *http.Request) string {
	bucket := BucketName(r)
	if bucket == "" {
		return ""
	}
	path := r.URL.Path
	prefix := "/" + bucket + "/"
	if strings.HasPrefix(path, prefix) {
		return path[len(prefix):]
	}
	return strings.TrimPrefix(chi.URLParam(r, "*"), "/")
}

// NewAPI composes the S3 API from its groups: a Dispatcher with every group
// mounted, ready to be registered on the server router.
func NewAPI(enc httpx.Encoder, groups ...Group) Routes {
	d := NewDispatcher(enc)
	for _, g := range groups {
		if g != nil {
			g.Mount(d)
		}
	}
	return d
}
