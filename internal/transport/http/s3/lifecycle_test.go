package s3_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	ilmmock "github.com/r3n3r0/nexo/internal/service/ilm/mock"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

// lifecycleFixture is the S3 API with the bucket routes and the lifecycle
// routes over mocks.
type lifecycleFixture struct {
	buckets   *bucketmock.Service
	lifecycle *ilmmock.Service
	policy    *policymock.Service
	routes    s3.LifecycleRoutes
	handler   http.Handler
}

func newLifecycleFixture(t *testing.T, allow bool) *lifecycleFixture {
	t.Helper()
	f := &lifecycleFixture{buckets: bucketmock.NewService(t), lifecycle: ilmmock.NewService(t), policy: policymock.NewService(t)}
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Allowed: allow, Reason: "test"}, nil).Maybe()
	f.policy.EXPECT().Simulate(mock.Anything, mock.Anything).Return(policy.Decision{}, nil).Maybe()
	enc := httpx.New()
	clk := clock.NewFake(time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC))
	f.routes = s3.NewLifecycleRoutes(s3.LifecycleDeps{Buckets: f.buckets, Lifecycle: f.lifecycle, Policy: f.policy, Enc: enc, Clock: clk})
	api := s3.NewAPI(s3.APIDeps{Enc: enc},
		s3.NewBucketRoutes(s3.BucketDeps{Buckets: f.buckets, Objects: objectmock.NewService(t), Policy: f.policy, Enc: enc, Clock: clk}),
		f.routes,
	)
	r := chi.NewRouter()
	api.Register(r)
	f.handler = withRoot(r)
	return f
}

// withRoot injects the root identity (the auth middleware's job).
func withRoot(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(middleware.WithIdentity(r.Context(), &domain.Identity{AccessKey: "root", IsRoot: true})))
	})
}

const lifecycleDoc = `<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`

func TestLifecycleRoutes(t *testing.T) {
	f := newLifecycleFixture(t, true)
	canonical := []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + lifecycleDoc)

	f.lifecycle.EXPECT().Validate(mock.Anything, []byte(lifecycleDoc)).Return(canonical, nil).Once()
	f.buckets.EXPECT().SetLifecycle(mock.Anything, "photos", canonical).Return(nil).Once()
	rec := serve(f.handler, request(http.MethodPut, "/photos?lifecycle", lifecycleDoc))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	f.buckets.EXPECT().GetLifecycle(mock.Anything, "photos").Return(canonical, nil).Once()
	rec = serve(f.handler, request(http.MethodGet, "/photos?lifecycle", ""))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, string(canonical), rec.Body.String())
	assert.Equal(t, httpx.ContentTypeXML, rec.Header().Get("Content-Type"))

	f.buckets.EXPECT().GetLifecycle(mock.Anything, "none").Return(nil,
		domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchLifecycleConfiguration", "The lifecycle configuration does not exist")).Once()
	requireError(t, serve(f.handler, request(http.MethodGet, "/none?lifecycle", "")), http.StatusNotFound, "NoSuchLifecycleConfiguration")

	f.buckets.EXPECT().DeleteLifecycle(mock.Anything, "photos").Return(nil).Once()
	assert.Equal(t, http.StatusNoContent, serve(f.handler, request(http.MethodDelete, "/photos?lifecycle", "")).Code)

	f.lifecycle.EXPECT().Validate(mock.Anything, []byte("<LifecycleConfiguration/>")).Return(nil,
		domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "no rule")).Once()
	requireError(t, serve(f.handler, request(http.MethodPut, "/photos?lifecycle", "<LifecycleConfiguration/>")), http.StatusBadRequest, "MalformedXML")

	f.lifecycle.EXPECT().Validate(mock.Anything, mock.Anything).Return(nil,
		domain.NewS3Error(domain.ErrNotImplemented, "NotImplemented", "tiering")).Once()
	requireError(t, serve(f.handler, request(http.MethodPut, "/photos?lifecycle", "<LifecycleConfiguration/>")), http.StatusNotImplemented, "NotImplemented")

	requireError(t, serve(f.handler, request(http.MethodPut, "/photos?lifecycle", lifecycleDoc, "Content-MD5", "AAAAAAAAAAAAAAAAAAAAAA==")),
		http.StatusBadRequest, "BadDigest")
}

func TestLifecycleRoutesAuthorization(t *testing.T) {
	f := newLifecycleFixture(t, false)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		requireError(t, serve(f.handler, request(method, "/photos?lifecycle", lifecycleDoc)), http.StatusForbidden, "AccessDenied")
	}
}

func TestLifecycleRoutesUnavailable(t *testing.T) {
	r := chi.NewRouter()
	s3.NewLifecycleRoutes(s3.LifecycleDeps{}).Register(r)
	requireError(t, serve(r, request(http.MethodPut, "/b?lifecycle", lifecycleDoc)), http.StatusNotImplemented, "NotImplemented")
	h := s3.NewLifecycleRoutes(s3.LifecycleDeps{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	assert.Equal(t, http.StatusTeapot, serve(h.Wrap(next), request(http.MethodGet, "/b/k", "")).Code, "the middleware passes through")
}

// TestLifecycleExpirationHeader: the object responses carry
// x-amz-expiration (docs/02 S-BKT-07).
func TestLifecycleExpirationHeader(t *testing.T) {
	lc := ilmmock.NewService(t)
	routes := s3.NewLifecycleRoutes(s3.LifecycleDeps{
		Buckets: bucketmock.NewService(t), Lifecycle: lc, Policy: policymock.NewService(t),
	}, s3.WithDomains("s3.example.com"))
	expiry := ilm.Expiry{Date: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), RuleID: "tmp-rule"}
	const want = `expiry-date="Mon, 12 Oct 2026 00:00:00 GMT", rule-id="tmp-rule"`

	// handler stands for the object routes: it records the operation as
	// the Dispatcher does and answers with status.
	handler := func(op string, status int, versionID string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpx.SetOperationName(r.Context(), op)
			if versionID != "" {
				w.Header().Set(httpx.HeaderAmzVersionID, versionID)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte("body"))
		})
	}
	withHolder := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, _ := httpx.WithOperationHolder(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	tests := []struct {
		name      string
		req       *http.Request
		op        string
		status    int
		versionID string
		expect    func()
		want      string
	}{
		{
			name: "get", req: request(http.MethodGet, "/b/tmp/a.txt", ""), op: "GetObject", status: http.StatusOK, versionID: "v1",
			expect: func() { lc.EXPECT().Expiration(mock.Anything, "b", "tmp/a.txt", "v1").Return(expiry, nil).Once() }, want: want,
		},
		{
			name: "range get", req: request(http.MethodGet, "/b/k", ""), op: "GetObject", status: http.StatusPartialContent,
			expect: func() { lc.EXPECT().Expiration(mock.Anything, "b", "k", "").Return(expiry, nil).Once() }, want: want,
		},
		{
			name: "head by version", req: request(http.MethodHead, "/b/k?versionId=v9", ""), op: "HeadObject", status: http.StatusOK,
			expect: func() { lc.EXPECT().Expiration(mock.Anything, "b", "k", "v9").Return(expiry, nil).Once() }, want: want,
		},
		{
			name: "put, virtual host", req: request(http.MethodPut, "http://b.s3.example.com/k", "x"), op: "PutObject", status: http.StatusOK,
			expect: func() { lc.EXPECT().Expiration(mock.Anything, "b", "k", "").Return(expiry, nil).Once() }, want: want,
		},
		{
			name: "complete multipart", req: request(http.MethodPost, "/b/k?uploadId=u", "<x/>"), op: "CompleteMultipartUpload", status: http.StatusOK,
			expect: func() { lc.EXPECT().Expiration(mock.Anything, "b", "k", "").Return(expiry, nil).Once() }, want: want,
		},
		{
			name: "no rule", req: request(http.MethodGet, "/b/k", ""), op: "GetObject", status: http.StatusOK,
			expect: func() { lc.EXPECT().Expiration(mock.Anything, "b", "k", "").Return(ilm.Expiry{}, nil).Once() },
		},
		{
			name: "lookup failure", req: request(http.MethodGet, "/b/k", ""), op: "GetObject", status: http.StatusOK,
			expect: func() {
				lc.EXPECT().Expiration(mock.Anything, "b", "k", "").Return(ilm.Expiry{}, domain.ErrReadQuorum).Once()
			},
		},
		{name: "not found", req: request(http.MethodGet, "/b/k", ""), op: "GetObject", status: http.StatusNotFound},
		{name: "tagging", req: request(http.MethodGet, "/b/k?tagging", ""), op: "GetObjectTagging", status: http.StatusOK},
		{name: "upload part", req: request(http.MethodPut, "/b/k?uploadId=u&partNumber=1", "x"), op: "UploadPart", status: http.StatusOK},
		{name: "bucket", req: request(http.MethodGet, "/b", ""), op: "ListObjects", status: http.StatusOK},
		{name: "system path", req: request(http.MethodGet, "/nexo/health/live", ""), status: http.StatusOK},
		{name: "delete", req: request(http.MethodDelete, "/b/k", ""), op: "DeleteObject", status: http.StatusNoContent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.expect != nil {
				tc.expect()
			}
			rec := serve(withHolder(routes.Wrap(handler(tc.op, tc.status, tc.versionID))), tc.req)
			assert.Equal(t, tc.status, rec.Code)
			assert.Equal(t, tc.want, rec.Header().Get("x-amz-expiration"))
		})
	}

	// Without an operation holder the shape of the request decides.
	lc.EXPECT().Expiration(mock.Anything, "b", "k", "").Return(expiry, nil).Once()
	rec := serve(routes.Wrap(handler("", http.StatusOK, "")), request(http.MethodGet, "/b/k?response-content-type=text/plain", ""))
	assert.Equal(t, want, rec.Header().Get("x-amz-expiration"))
	rec = serve(routes.Wrap(handler("", http.StatusOK, "")), request(http.MethodGet, "/b/k?acl", ""))
	assert.Empty(t, rec.Header().Get("x-amz-expiration"))
}
