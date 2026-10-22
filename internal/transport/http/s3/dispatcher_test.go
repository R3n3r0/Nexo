package s3_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/health"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	httpxmock "github.com/r3n3r0/nexo/internal/transport/http/httpx/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

// recorded is what a test handler saw.
type recorded struct {
	op, bucket, key string
}

// recordingDispatcher registers a recording handler for every operation.
func recordingDispatcher(t *testing.T, seen *recorded, ops []s3.Operation, opts ...s3.Option) http.Handler {
	t.Helper()
	d := s3.NewDispatcher(s3.DispatcherDeps{}, opts...)
	for _, op := range ops {
		d.Handle(op, func(w http.ResponseWriter, r *http.Request) {
			got, ok := s3.OperationFromContext(r.Context())
			require.True(t, ok)
			*seen = recorded{op: got.Name, bucket: s3.BucketName(r), key: s3.ObjectKey(r)}
			w.WriteHeader(http.StatusOK)
		})
	}
	r := chi.NewRouter()
	d.Register(r)
	return r
}

func dispatcherOps() []s3.Operation {
	return []s3.Operation{
		// The presigned operation is registered first: precedence does not
		// depend on the registration order.
		{Name: "PresignedGetObject", Method: "GET", Scope: s3.ScopeObject, Query: "X-Amz-Signature"},
		{Name: "PresignedPutObject", Method: "PUT", Scope: s3.ScopeObject, Query: "X-Amz-Signature"},
		{Name: "ListBuckets", Method: "GET", Scope: s3.ScopeService},
		{Name: "ListObjects", Method: "GET", Scope: s3.ScopeBucket},
		{Name: "ListObjectsV2", Method: "GET", Scope: s3.ScopeBucket, Query: "list-type", QueryValue: "2"},
		{Name: "ListMultipartUploads", Method: "GET", Scope: s3.ScopeBucket, Query: "uploads"},
		{Name: "ListObjectVersions", Method: "GET", Scope: s3.ScopeBucket, Query: "versions"},
		{Name: "CreateBucket", Method: "PUT", Scope: s3.ScopeBucket},
		{Name: "GetObject", Method: "GET", Scope: s3.ScopeObject},
		{Name: "GetObjectTagging", Method: "GET", Scope: s3.ScopeObject, Query: "tagging"},
		{Name: "ListParts", Method: "GET", Scope: s3.ScopeObject, Query: "uploadId"},
		{Name: "PutObject", Method: "PUT", Scope: s3.ScopeObject},
		{Name: "UploadPart", Method: "PUT", Scope: s3.ScopeObject, Query: "uploadId"},
		{Name: "CreateMultipartUpload", Method: "POST", Scope: s3.ScopeObject, Query: "uploads"},
	}
}

func TestDispatcherPathStyle(t *testing.T) {
	var seen recorded
	h := recordingDispatcher(t, &seen, dispatcherOps())
	tests := []struct {
		method, target string
		want           recorded
	}{
		{"GET", "/", recorded{"ListBuckets", "", ""}},
		{"GET", "/b", recorded{"ListObjects", "b", ""}},
		{"GET", "/b/", recorded{"ListObjects", "b", ""}},
		{"GET", "/b?list-type=2", recorded{"ListObjectsV2", "b", ""}},
		{"GET", "/b?list-type=1", recorded{"ListObjects", "b", ""}},
		{"GET", "/b?uploads", recorded{"ListMultipartUploads", "b", ""}},
		{"GET", "/b?versions&prefix=a", recorded{"ListObjectVersions", "b", ""}},
		{"PUT", "/b", recorded{"CreateBucket", "b", ""}},
		{"GET", "/b/k", recorded{"GetObject", "b", "k"}},
		{"GET", "/b/dir/sub/k%20space.txt", recorded{"GetObject", "b", "dir/sub/k space.txt"}},
		{"GET", "/b/dir/", recorded{"GetObject", "b", "dir/"}},
		{"GET", "/b/a%2Fb%3Fc", recorded{"GetObject", "b", "a/b?c"}},
		{"GET", "/b/k?partNumber=2", recorded{"GetObject", "b", "k"}},
		{"GET", "/b/k?uploadId=x", recorded{"ListParts", "b", "k"}},
		{"GET", "/b/k?tagging", recorded{"GetObjectTagging", "b", "k"}},
		{"POST", "/b/k?uploads", recorded{"CreateMultipartUpload", "b", "k"}},
		// Presigned requests rank above the default operation but below the
		// sub-resources.
		{"GET", "/b/k?X-Amz-Signature=s", recorded{"PresignedGetObject", "b", "k"}},
		{"GET", "/b/k?tagging&X-Amz-Signature=s", recorded{"GetObjectTagging", "b", "k"}},
		{"PUT", "/b/k?uploadId=u&partNumber=1&X-Amz-Signature=s", recorded{"UploadPart", "b", "k"}},
		{"PUT", "/b/k?X-Amz-Signature=s", recorded{"PresignedPutObject", "b", "k"}},
		{"GET", "/b?list-type=2&X-Amz-Signature=s", recorded{"ListObjectsV2", "b", ""}},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			seen = recorded{}
			rec := serve(h, httptest.NewRequest(tc.method, tc.target, nil))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, tc.want, seen)
		})
	}
}

func TestDispatcherVirtualHost(t *testing.T) {
	var seen recorded
	h := recordingDispatcher(t, &seen, dispatcherOps(), s3.WithDomains("S3.Example.com:9000", "", ".cdn.example.org."))
	tests := []struct {
		method, host, target string
		want                 recorded
	}{
		{"GET", "photos.s3.example.com:9000", "/", recorded{"ListObjects", "photos", ""}},
		{"GET", "photos.s3.example.com", "/?list-type=2", recorded{"ListObjectsV2", "photos", ""}},
		{"GET", "PHOTOS.S3.EXAMPLE.COM", "/a/b.txt", recorded{"GetObject", "photos", "a/b.txt"}},
		{"GET", "my.dotted.bucket.s3.example.com", "/k", recorded{"GetObject", "my.dotted.bucket", "k"}},
		{"PUT", "new.cdn.example.org", "/", recorded{"CreateBucket", "new", ""}},
		{"PUT", "photos.s3.example.com", "/k?uploadId=u&partNumber=1", recorded{"UploadPart", "photos", "k"}},
		// The domain itself and unknown hosts are path-style.
		{"GET", "s3.example.com:9000", "/", recorded{"ListBuckets", "", ""}},
		{"GET", "s3.example.com", "/b/k", recorded{"GetObject", "b", "k"}},
		{"GET", "127.0.0.1:9000", "/b/k", recorded{"GetObject", "b", "k"}},
		{"GET", "other.com", "/b", recorded{"ListObjects", "b", ""}},
	}
	for _, tc := range tests {
		t.Run(tc.host+tc.target, func(t *testing.T) {
			seen = recorded{}
			req := httptest.NewRequest(tc.method, tc.target, nil)
			req.Host = tc.host
			rec := serve(h, req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, tc.want, seen)
		})
	}
}

func TestDispatcherErrors(t *testing.T) {
	var seen recorded
	h := recordingDispatcher(t, &seen, dispatcherOps())
	tests := []struct {
		name, method, target string
		status               int
		code                 string
	}{
		{"no default operation", "POST", "/b/k", http.StatusMethodNotAllowed, "MethodNotAllowed"},
		{"presigned, no default operation", "POST", "/b/k?X-Amz-Signature=x", http.StatusMethodNotAllowed, "MethodNotAllowed"},
		{"unknown sub-resource", "POST", "/b/k?unknown", http.StatusNotImplemented, "NotImplemented"},
		{"no operation for the method", "DELETE", "/b/k", http.StatusMethodNotAllowed, "MethodNotAllowed"},
		{"unknown method", "PROPFIND", "/b/k", http.StatusMethodNotAllowed, "MethodNotAllowed"},
		{"service scope", "PUT", "/", http.StatusMethodNotAllowed, "MethodNotAllowed"},
		{"key too long", "GET", "/b/" + strings.Repeat("k", domain.MaxObjectKeyLength+1), http.StatusBadRequest, "KeyTooLongError"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(h, httptest.NewRequest(tc.method, tc.target, nil))
			requireError(t, rec, tc.status, tc.code)
		})
	}
	// A key of exactly the maximum length is served.
	rec := serve(h, httptest.NewRequest("GET", "/b/"+strings.Repeat("k", domain.MaxObjectKeyLength), nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	d := s3.NewDispatcher(s3.DispatcherDeps{Enc: httpx.New()})
	ok := func(w http.ResponseWriter, _ *http.Request) {}
	d.Handle(s3.Operation{Name: "A", Method: "GET", Scope: s3.ScopeBucket, Query: "uploads"}, ok)
	assert.Panics(t, func() {
		d.Handle(s3.Operation{Name: "Dup", Method: "GET", Scope: s3.ScopeBucket, Query: "uploads"}, ok)
	}, "registering an operation twice is a wiring error")
	assert.Panics(t, func() { d.Handle(s3.Operation{Name: "Nil", Method: "GET", Scope: s3.ScopeService}, nil) })
}

func TestDispatcherRouterFallbacks(t *testing.T) {
	enc := httpx.New()
	r := chi.NewRouter()
	s3.NewDispatcher(s3.DispatcherDeps{Enc: enc}).Register(r)
	s3.NewHealthRoutes(s3.HealthDeps{Health: health.New(health.Deps{}), Enc: enc}).Register(r)
	// A method the health route does not serve gets an S3 405.
	rec := serve(r, httptest.NewRequest(http.MethodPost, domain.HealthLivePath, nil))
	requireError(t, rec, http.StatusMethodNotAllowed, "MethodNotAllowed")
	// The dispatcher owns every other path.
	rec = serve(r, httptest.NewRequest(http.MethodGet, "/nexo/health", nil))
	requireError(t, rec, http.StatusMethodNotAllowed, "MethodNotAllowed")
}

// recordingGroup mounts a recording handler for the given operations.
type recordingGroup struct {
	seen *recorded
	ops  []s3.Operation
}

func (g recordingGroup) Mount(d s3.Dispatcher) {
	for _, op := range g.ops {
		d.Handle(op, func(w http.ResponseWriter, r *http.Request) {
			got, _ := s3.OperationFromContext(r.Context())
			*g.seen = recorded{op: got.Name, bucket: s3.BucketName(r), key: s3.ObjectKey(r)}
			w.WriteHeader(http.StatusOK)
		})
	}
}

// TestAPISystemRoutesArePathStyleOnly is a regression test: the health
// and metrics routes answer path-style requests only; for a virtual-host
// request their paths are object keys of the bucket.
func TestAPISystemRoutesArePathStyleOnly(t *testing.T) {
	enc := httpx.New()
	var seen recorded
	group := recordingGroup{seen: &seen, ops: []s3.Operation{
		{Name: "GetObject", Method: http.MethodGet, Scope: s3.ScopeObject},
		{Name: "HeadObject", Method: http.MethodHead, Scope: s3.ScopeObject},
		{Name: "PutObject", Method: http.MethodPut, Scope: s3.ScopeObject},
		{Name: "CreateMultipartUpload", Method: http.MethodPost, Scope: s3.ScopeObject, Query: "uploads"},
	}}
	mr, err := s3.NewMetricsRoutes(s3.MetricsDeps{Registry: metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())}, s3.WithPublicMetrics(true))
	require.NoError(t, err)
	// cdn.custom.test is the custom host of an s3 distribution.
	hostBuckets := httpxmock.NewHostBucketResolver(t)
	hostBuckets.EXPECT().BucketOfHost(tmock.Anything).RunAndReturn(func(host string) (string, bool) {
		return "site", strings.HasPrefix(host, "cdn.custom.test")
	}).Maybe()
	api := s3.NewAPI(s3.APIDeps{Enc: enc, Domains: []string{"example.com"}, HostBuckets: hostBuckets, System: []s3.Routes{
		s3.NewHealthRoutes(s3.HealthDeps{Health: health.New(health.Deps{}), Enc: enc}),
		mr,
	}}, group)
	r := chi.NewRouter()
	api.Register(r)

	tests := []struct {
		name, method, host, target string
		want                       recorded // zero: a system route answered
	}{
		{name: "path-style health", method: http.MethodGet, host: "localhost", target: domain.HealthLivePath},
		{name: "path-style metrics", method: http.MethodGet, host: "localhost", target: s3.PathMetricsNode},
		{name: "virtual-host health key", method: http.MethodGet, host: "bkt.example.com", target: domain.HealthLivePath,
			want: recorded{op: "GetObject", bucket: "bkt", key: strings.TrimPrefix(domain.HealthLivePath, "/")}},
		{name: "virtual-host head health key", method: http.MethodHead, host: "bkt.example.com:9000", target: domain.HealthReadyPath,
			want: recorded{op: "HeadObject", bucket: "bkt", key: strings.TrimPrefix(domain.HealthReadyPath, "/")}},
		{name: "virtual-host metrics key", method: http.MethodGet, host: "bkt.example.com", target: s3.PathMetricsCluster,
			want: recorded{op: "GetObject", bucket: "bkt", key: strings.TrimPrefix(s3.PathMetricsCluster, "/")}},
		{name: "virtual-host put on a system path", method: http.MethodPut, host: "bkt.example.com", target: s3.PathMetricsNode,
			want: recorded{op: "PutObject", bucket: "bkt", key: strings.TrimPrefix(s3.PathMetricsNode, "/")}},
		{name: "virtual-host post on a system path", method: http.MethodPost, host: "bkt.example.com", target: domain.HealthLivePath + "?uploads",
			want: recorded{op: "CreateMultipartUpload", bucket: "bkt", key: strings.TrimPrefix(domain.HealthLivePath, "/")}},
		// The custom host of an s3 distribution never takes the reserved
		// paths: whatever host a load balancer probes, the health and the
		// metrics answer (regression: an alias captured the probes).
		{name: "custom host health", method: http.MethodGet, host: "cdn.custom.test:9000", target: domain.HealthReadyPath},
		{name: "custom host metrics", method: http.MethodGet, host: "cdn.custom.test", target: s3.PathMetricsNode},
		{name: "custom host object", method: http.MethodGet, host: "cdn.custom.test", target: "/index.html",
			want: recorded{op: "GetObject", bucket: "site", key: "index.html"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen = recorded{}
			req := httptest.NewRequest(tt.method, tt.target, nil)
			req.Host = tt.host
			rec := serve(r, req)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, tt.want, seen)
		})
	}
}

func TestMetricsRequest(t *testing.T) {
	match := s3.MetricsRequest("Example.COM:9000", "")
	tests := []struct {
		host, path string
		want       bool
	}{
		{host: "localhost:9000", path: s3.PathMetricsNode, want: true},
		{host: "example.com", path: s3.PathMetricsCluster, want: true},
		{host: "localhost", path: "/nexo/metricsx", want: false},
		{host: "localhost", path: "/bkt/nexo/metrics/v1/node", want: false},
		{host: "bkt.example.com", path: s3.PathMetricsNode, want: false},
		{host: "bkt.EXAMPLE.com:9000", path: s3.MetricsPathPrefix + "foo", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.host+tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Host = tt.host
			assert.Equal(t, tt.want, match(req))
		})
	}
	assert.True(t, s3.MetricsRequest()(httptest.NewRequest(http.MethodGet, s3.PathMetricsNode, nil)), "without domains every request is path-style")
}

func TestSigningService(t *testing.T) {
	service := s3.SigningService("example.com")
	tests := []struct {
		method, host, target, want string
	}{
		{method: http.MethodPost, host: "localhost:9000", target: "/", want: auth.ServiceSTS},
		{method: http.MethodPost, host: "example.com", target: "/?Action=AssumeRole", want: auth.ServiceSTS},
		{method: http.MethodGet, host: "localhost", target: "/", want: auth.ServiceS3},
		{method: http.MethodPost, host: "localhost", target: "/bkt?delete", want: auth.ServiceS3},
		{method: http.MethodPost, host: "bkt.example.com", target: "/?delete", want: auth.ServiceS3},
		{method: http.MethodPut, host: "localhost", target: "/bkt/key", want: auth.ServiceS3},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.host+tt.target, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, nil)
			req.Host = tt.host
			assert.Equal(t, tt.want, service(req))
		})
	}
}

func TestBucketNameAndKeyWithoutDispatcher(t *testing.T) {
	r := chi.NewRouter()
	var bucket, key string
	r.Get("/{bucket}/*", func(w http.ResponseWriter, req *http.Request) {
		bucket, key = s3.BucketName(req), s3.ObjectKey(req)
	})
	r.Get("/x/{bucket}/*", func(w http.ResponseWriter, req *http.Request) {
		bucket, key = s3.BucketName(req), s3.ObjectKey(req)
	})
	serve(r, httptest.NewRequest(http.MethodGet, "/b/dir/k", nil))
	assert.Equal(t, "b", bucket)
	assert.Equal(t, "dir/k", key)
	serve(r, httptest.NewRequest(http.MethodGet, "/x/b/k2", nil))
	assert.Equal(t, "b", bucket)
	assert.Equal(t, "k2", key, "falls back to the wildcard parameter")
	assert.Empty(t, s3.ObjectKey(httptest.NewRequest(http.MethodGet, "/", nil)))
	_, ok := s3.OperationFromContext(httptest.NewRequest(http.MethodGet, "/", nil).Context())
	assert.False(t, ok)
}

func TestAPIWithoutServicesAnswersNotImplemented(t *testing.T) {
	enc := httpx.New()
	api := s3.NewAPI(s3.APIDeps{Enc: enc},
		s3.NewBucketRoutes(s3.BucketDeps{Enc: enc}),
		s3.NewObjectRoutes(s3.ObjectDeps{Enc: enc}),
		s3.NewMultipartRoutes(s3.MultipartDeps{Enc: enc}),
		s3.NewPresignedRoutes(s3.PresignedDeps{Enc: enc}),
		s3.NewPostPolicyRoutes(s3.PostPolicyDeps{Enc: enc}),
		s3.NewLifecycleRoutes(s3.LifecycleDeps{Enc: enc}),
		s3.NewNotificationRoutes(s3.NotificationDeps{Enc: enc}),
		nil,
	)
	r := chi.NewRouter()
	api.Register(r)
	s3.NewHealthRoutes(s3.HealthDeps{Health: health.New(health.Deps{}), Enc: enc}).Register(r)
	mr, err := s3.NewMetricsRoutes(s3.MetricsDeps{Registry: metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())}, s3.WithPublicMetrics(true))
	require.NoError(t, err)
	mr.Register(r)
	for _, tc := range []struct{ method, target string }{
		{"GET", "/"}, {"PUT", "/bucket"}, {"HEAD", "/bucket"}, {"GET", "/bucket?list-type=2&prefix=a"},
		{"GET", "/bucket?versions"}, {"GET", "/bucket?uploads"}, {"POST", "/bucket?delete"}, {"POST", "/bucket"},
		{"PUT", "/bucket/key"}, {"GET", "/bucket/dir/key.txt"}, {"POST", "/bucket/key?uploads"},
		{"PUT", "/bucket/key?uploadId=1&partNumber=2"}, {"GET", "/bucket/key?tagging"},
		{"GET", "/bucket/key?X-Amz-Signature=abc&X-Amz-Algorithm=AWS4-HMAC-SHA256"}, {"GET", "/bucket/key?retention"},
		{"GET", "/bucket?website"}, {"PUT", "/bucket?lifecycle"}, {"GET", "/bucket?notification"},
	} {
		rec := serve(r, httptest.NewRequest(tc.method, tc.target, nil))
		assert.Equal(t, http.StatusNotImplemented, rec.Code, "%s %s", tc.method, tc.target)
		if tc.method != http.MethodHead {
			assert.Equal(t, "NotImplemented", s3Error(t, rec).Code, "%s %s", tc.method, tc.target)
		}
	}
	// Health and metrics paths win over the dispatcher.
	rec := serve(r, httptest.NewRequest(http.MethodGet, domain.HealthLivePath, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	rec = serve(r, httptest.NewRequest(http.MethodGet, s3.PathMetricsNode, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
	rec = serve(r, httptest.NewRequest(http.MethodGet, "/nexo", nil))
	assert.Equal(t, http.StatusNotImplemented, rec.Code, "a bucket literally named nexo still routes to S3")
	_, err = s3.NewMetricsRoutes(s3.MetricsDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "the registry is wired by app, never created here")
}

func TestGroupsRegisterStandalone(t *testing.T) {
	enc := httpx.New()
	for name, g := range map[string]s3.Routes{
		"bucket":     s3.NewBucketRoutes(s3.BucketDeps{}),
		"object":     s3.NewObjectRoutes(s3.ObjectDeps{}),
		"multipart":  s3.NewMultipartRoutes(s3.MultipartDeps{}),
		"presigned":  s3.NewPresignedRoutes(s3.PresignedDeps{}),
		"postpolicy": s3.NewPostPolicyRoutes(s3.PostPolicyDeps{Enc: enc}),
	} {
		r := chi.NewRouter()
		g.Register(r)
		rec := serve(r, httptest.NewRequest(http.MethodGet, "/b/k", nil))
		assert.Contains(t, []int{http.StatusNotImplemented, http.StatusMethodNotAllowed}, rec.Code, name)
		assert.Equal(t, httpx.ContentTypeXML, rec.Header().Get("Content-Type"), name)
	}
}

func TestUnsupportedSubResources(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct{ method, target string }{
		{"GET", "/b?website"}, {"PUT", "/b?website"}, {"DELETE", "/b?website"},
		{"GET", "/b?logging"}, {"GET", "/b?accelerate"}, {"GET", "/b?publicAccessBlock"},
		{"GET", "/b/k?torrent"},
		{"POST", "/b/k?restore"}, {"POST", "/b/k?select&select-type=2"},
	} {
		rec := f.do(tc.method, tc.target, "<x/>")
		requireError(t, rec, http.StatusNotImplemented, "NotImplemented")
	}
}
