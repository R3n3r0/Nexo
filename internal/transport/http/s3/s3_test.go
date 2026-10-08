package s3

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/health"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

func newAPI(t *testing.T) http.Handler {
	t.Helper()
	enc := httpx.New()
	api := NewAPI(enc,
		NewBucketRoutes(BucketDeps{Enc: enc}),
		NewObjectRoutes(ObjectDeps{Enc: enc}),
		NewMultipartRoutes(MultipartDeps{Enc: enc}),
		NewPresignedRoutes(PresignedDeps{Enc: enc}),
		NewPostPolicyRoutes(PostPolicyDeps{Enc: enc}),
		nil,
	)
	r := chi.NewRouter()
	api.Register(r)
	NewHealthRoutes(HealthDeps{Health: health.New(health.Deps{}), Enc: enc}).Register(r)
	NewMetricsRoutes(MetricsDeps{Registry: metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())}).Register(r)
	return r
}

func do(t *testing.T, h http.Handler, method, target string) (*httptest.ResponseRecorder, httpx.S3ErrorResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	var body httpx.S3ErrorResponse
	if rec.Body.Len() > 0 && strings.Contains(rec.Header().Get("Content-Type"), "xml") {
		require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &body))
	}
	return rec, body
}

func TestAPIAnswersS3XMLNotImplemented(t *testing.T) {
	h := newAPI(t)
	for _, tc := range []struct{ method, target string }{
		{"GET", "/"},
		{"PUT", "/bucket"},
		{"HEAD", "/bucket"},
		{"GET", "/bucket?list-type=2&prefix=a"},
		{"GET", "/bucket?versions"},
		{"GET", "/bucket?uploads"},
		{"POST", "/bucket?delete"},
		{"POST", "/bucket"},
		{"PUT", "/bucket/key"},
		{"GET", "/bucket/dir/key.txt"},
		{"POST", "/bucket/key?uploads"},
		{"PUT", "/bucket/key?uploadId=1&partNumber=2"},
		{"GET", "/bucket/key?tagging"},
		{"GET", "/bucket/key?X-Amz-Signature=abc&X-Amz-Algorithm=AWS4-HMAC-SHA256"},
	} {
		rec, body := do(t, h, tc.method, tc.target)
		assert.Equal(t, http.StatusNotImplemented, rec.Code, "%s %s", tc.method, tc.target)
		if tc.method != http.MethodHead {
			assert.Equal(t, "NotImplemented", body.Code, "%s %s", tc.method, tc.target)
			assert.Equal(t, httpx.ContentTypeXML, rec.Header().Get("Content-Type"))
		}
	}
}

func TestDispatcherSelectsOperation(t *testing.T) {
	enc := httpx.New()
	d := NewDispatcher(enc)
	var seen []string
	record := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			op, ok := OperationFromContext(r.Context())
			require.True(t, ok)
			assert.Equal(t, name, op.Name)
			seen = append(seen, name+":"+BucketName(r)+":"+ObjectKey(r))
			w.WriteHeader(http.StatusOK)
		}
	}
	d.Handle(Operation{Name: "ListObjects", Method: "GET", Scope: ScopeBucket}, record("ListObjects"))
	d.Handle(Operation{Name: "ListObjectsV2", Method: "GET", Scope: ScopeBucket, Query: "list-type", QueryValue: "2"}, record("ListObjectsV2"))
	d.Handle(Operation{Name: "ListMultipartUploads", Method: "GET", Scope: ScopeBucket, Query: "uploads"}, record("ListMultipartUploads"))
	d.Handle(Operation{Name: "GetObject", Method: "GET", Scope: ScopeObject}, record("GetObject"))
	d.Handle(Operation{Name: "ListParts", Method: "GET", Scope: ScopeObject, Query: "uploadId"}, record("ListParts"))
	d.Handle(Operation{Name: "Uploads", Method: "POST", Scope: ScopeObject, Query: "uploads"}, record("Uploads"))

	r := chi.NewRouter()
	d.Register(r)

	for _, target := range []string{
		"/b", "/b/", "/b?list-type=2", "/b?list-type=1", "/b?uploads",
		"/b/k", "/b/dir/sub/k%20space.txt", "/b/k?uploadId=x",
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		assert.Equal(t, http.StatusOK, rec.Code, target)
	}
	assert.Equal(t, []string{
		"ListObjects:b:", "ListObjects:b:", "ListObjectsV2:b:", "ListObjects:b:", "ListMultipartUploads:b:",
		"GetObject:b:k", "GetObject:b:dir/sub/k space.txt", "ListParts:b:k",
	}, seen)

	// POST on an object without ?uploads: no default operation → NotImplemented.
	rec, body := do(t, r, http.MethodPost, "/b/k")
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Equal(t, "NotImplemented", body.Code)

	// Method with no operations at all → MethodNotAllowed.
	rec, body = do(t, r, http.MethodDelete, "/b/k")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "MethodNotAllowed", body.Code)

	// Unknown verbs are rejected by chi's MethodNotAllowed with S3 XML.
	_, body = do(t, r, "PROPFIND", "/b/k")
	assert.Equal(t, "MethodNotAllowed", body.Code)

	// Duplicate registration is a wiring error.
	assert.Panics(t, func() {
		d.Handle(Operation{Name: "Dup", Method: "GET", Scope: ScopeBucket, Query: "uploads"}, record("Dup"))
	})
	assert.Panics(t, func() { d.Handle(Operation{Name: "Nil", Method: "GET", Scope: ScopeService}, nil) })
}

func TestGroupsRegisterStandalone(t *testing.T) {
	enc := httpx.New()
	for name, g := range map[string]Routes{
		"bucket":     NewBucketRoutes(BucketDeps{}),
		"object":     NewObjectRoutes(ObjectDeps{}),
		"multipart":  NewMultipartRoutes(MultipartDeps{}),
		"presigned":  NewPresignedRoutes(PresignedDeps{}),
		"postpolicy": NewPostPolicyRoutes(PostPolicyDeps{Enc: enc}),
	} {
		r := chi.NewRouter()
		g.Register(r)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/b/k", nil))
		assert.Contains(t, []int{http.StatusNotImplemented, http.StatusBadRequest}, rec.Code, name)
		assert.Equal(t, httpx.ContentTypeXML, rec.Header().Get("Content-Type"), name)
	}
}

func TestHealthRoutes(t *testing.T) {
	ready := true
	svc := health.New(health.Deps{Readiness: map[string]health.Check{"storage": func(context.Context) error {
		if ready {
			return nil
		}
		return errors.New("no quorum")
	}}})
	r := chi.NewRouter()
	NewHealthRoutes(HealthDeps{Health: svc}).Register(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathHealthLive, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var st health.Status
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	assert.True(t, st.OK)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, PathHealthLive, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathHealthReady, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	ready = false
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathHealthReady, nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "no quorum")

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathHealthCluster, nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var cs health.ClusterStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cs))
	assert.Equal(t, 1, cs.Nodes)
}

func TestMetricsRoutes(t *testing.T) {
	h := newAPI(t)
	for _, p := range []string{PathMetricsCluster, PathMetricsNode, PathMetricsBucket} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		assert.Equal(t, http.StatusOK, rec.Code, p)
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain", p)
	}
	// Health and metrics paths win over the /{bucket}/* pattern, while a
	// bucket literally named "nexo" still routes to S3.
	rec, body := do(t, h, http.MethodGet, "/nexo")
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Equal(t, "NotImplemented", body.Code)
}
