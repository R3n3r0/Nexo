// Package s3 implements the S3 API transport. S3 selects the operation from
// the HTTP method, the URL scope (service, bucket or object) and a
// discriminating query parameter (?uploads, ?tagging, ...), which a plain
// path router cannot express. The Dispatcher owns the URL space: it resolves
// the bucket and the key of path-style ("/{bucket}/{key...}") and
// virtual-host ("{bucket}.{domain}/{key...}") requests and selects the
// operation; the route groups (BucketRoutes, ObjectRoutes, ...) mount their
// operations on it.
//
// Every handler decodes the request, authorizes it with the policy engine
// (ActionOf maps each operation to its policy action), calls a service and
// encodes the response; the services own the business rules.
//
// Every group is also a Routes: registering it alone on a chi router mounts
// it on a private dispatcher, which is handy for tests.
package s3

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/settings"
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
	// audit and policy mapping (ActionOf).
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

// Dispatcher routes S3 requests to operations. Among the operations of a
// method and scope it selects, in order of precedence: the one whose Query
// and QueryValue match, the one whose sub-resource Query is present, the one
// discriminated by an authentication parameter (X-Amz-*, the presigned
// operations), the default operation (empty Query). Registration order
// breaks ties. A request matching no operation is answered NotImplemented,
// a method without operations MethodNotAllowed.
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
// string (X-Amz-Signature): they take precedence over the default
// operations of the same method and scope (not over the sub-resources) and
// delegate to the object/bucket handlers.
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

// BucketName returns the bucket of the request as resolved by the
// Dispatcher (path-style or virtual-host), falling back to the "bucket"
// route parameter for handlers mounted without it.
func BucketName(r *http.Request) string {
	if t, ok := targetFromContext(r.Context()); ok {
		return t.bucket
	}
	return chi.URLParam(r, "bucket")
}

// ObjectKey returns the decoded object key of the request as resolved by
// the Dispatcher, falling back to the path after "/{bucket}/".
func ObjectKey(r *http.Request) string {
	if t, ok := targetFromContext(r.Context()); ok {
		return t.key
	}
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

// APIDeps are the dependencies of the composed S3 API.
type APIDeps struct {
	// Enc writes the routing errors (default httpx.New()).
	Enc httpx.Encoder
	// Domains enable virtual-host style requests ("{bucket}.{domain}") for
	// each domain (NEXO_DOMAIN); without them every request is path-style.
	Domains []string
	// HostBuckets resolves the custom hosts of the buckets (the s3
	// distributions), before the domains; nil resolves none.
	HostBuckets httpx.HostBucketResolver
	// System are the routes the listener serves beside the S3 API under
	// the reserved "/nexo/" prefix (HealthRoutes, MetricsRoutes). They
	// answer path-style requests only: the path of a virtual-host request
	// is an object key of its bucket ("bkt.{domain}/nexo/metrics/x" is the
	// key "nexo/metrics/x"), which the Dispatcher serves.
	System []Routes
	// Buckets provides the CORS configurations of the buckets (S-BKT-06):
	// the cross-origin requests of a bucket with one are decided by its
	// rules. Nil applies the deployment-wide policy to every request.
	Buckets bucket.Service
	// Settings provides the deployment-wide CORS policy
	// (api.cors_allow_origin) of the buckets without a CORS configuration,
	// of the service-level requests and of the system routes. Nil allows
	// every origin.
	Settings settings.Service
	// Logger reports the CORS configurations that cannot be read
	// (default slog.Default()).
	Logger *slog.Logger
}

// api is the composed S3 API: the system routes, guarded against
// virtual-host requests, and the Dispatcher owning the rest of the URL
// space, both behind the CORS handling.
type api struct {
	d      *dispatcher
	system []Routes
	cors   *corsHandler
}

// NewAPI composes the S3 API from its groups: a Dispatcher with every group
// mounted and the system routes, ready to be registered on the server router.
func NewAPI(deps APIDeps, groups ...Group) Routes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	o := newOptions([]Option{WithDomains(deps.Domains...)})
	d := &dispatcher{enc: deps.Enc, domains: o.domains, hostBuckets: deps.HostBuckets, ops: make(map[opKey][]opEntry)}
	for _, g := range groups {
		if g != nil {
			g.Mount(d)
		}
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	cors := &corsHandler{buckets: deps.Buckets, settings: deps.Settings, enc: deps.Enc, logger: deps.Logger, resolve: d.corsBucket}
	return &api{d: d, system: deps.System, cors: cors}
}

// corsBucket returns the bucket whose CORS rules apply to a request: the
// one it addresses, none for the service-level requests and the system
// routes ("/nexo/..." in path-style).
func (d *dispatcher) corsBucket(r *http.Request) string {
	t := d.resolve(r)
	if t.virtualHost || t.bucket != domain.ReservedBucket {
		return t.bucket
	}
	return ""
}

// Register implements Routes.
func (a *api) Register(r chi.Router) {
	r = r.With(a.cors.wrap)
	if len(a.system) > 0 {
		guarded := r.With(a.pathStyleOnly)
		for _, rt := range a.system {
			if rt != nil {
				rt.Register(guarded)
			}
		}
	}
	a.d.Register(r)
}

// pathStyleOnly hands virtual-host requests matched by a system route to
// the Dispatcher: their path is an object key.
func (a *api) pathStyleOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.d.hostBucket(r); ok {
			a.d.serve(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SigningService returns the function telling the SigV4 service a request
// is addressed to (middleware.AuthDeps.Service): auth.ServiceSTS for the
// STS API (POST on the service scope "/", path-style), auth.ServiceS3 for
// every other request, virtual-host ones included ("bkt.{domain}/" is a
// bucket).
func SigningService(domains ...string) func(r *http.Request) string {
	normalized := newOptions([]Option{WithDomains(domains...)}).domains
	return func(r *http.Request) string {
		if r.Method != http.MethodPost || strings.TrimPrefix(r.URL.Path, "/") != "" {
			return auth.ServiceS3
		}
		if _, virtualHost := virtualHostBucket(normalized, r.Host); virtualHost {
			return auth.ServiceS3
		}
		return auth.ServiceSTS
	}
}

// MetricsRequest returns the predicate selecting the requests of the
// metrics routes: path-style requests under MetricsPathPrefix. A
// virtual-host request ("{bucket}.{domain}" for one of domains) is a bucket
// request whatever its path. It is the middleware.AuthDeps.Skip of the S3
// listener: the metrics routes authenticate their own bearer tokens, every
// other request goes through SigV4.
func MetricsRequest(domains ...string) func(r *http.Request) bool {
	normalized := newOptions([]Option{WithDomains(domains...)}).domains
	return func(r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, MetricsPathPrefix) {
			return false
		}
		_, virtualHost := virtualHostBucket(normalized, r.Host)
		return !virtualHost
	}
}

// VirtualHostRequest returns the predicate selecting the requests that
// address a bucket by host name (<bucket>.<domain>, for the given
// domains): their path is an object key, whatever it looks like (a key
// "nexo/rpc/v1/..." is not an internode call).
func VirtualHostRequest(domains ...string) func(r *http.Request) bool {
	normalized := newOptions([]Option{WithDomains(domains...)}).domains
	return func(r *http.Request) bool {
		_, virtualHost := virtualHostBucket(normalized, r.Host)
		return virtualHost
	}
}
