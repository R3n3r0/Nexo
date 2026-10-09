package s3_test

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
	ilmmock "github.com/r3n3r0/nexo/internal/service/ilm/mock"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// fixture is the full S3 API over mocked services.
type fixture struct {
	t        *testing.T
	objects  *objectmock.Service
	buckets  *bucketmock.Service
	policy   *policymock.Service
	verifier *authmock.Verifier
	ilm      *ilmmock.Service
	events   *eventmock.Service
	enc      httpx.Codec
	clock    *clock.Fake
	// identity is injected in every request (the auth middleware's job);
	// nil leaves the request without identity.
	identity *domain.Identity
	handler  http.Handler
}

type fixtureConfig struct {
	identity      *domain.Identity
	noIdentity    bool
	denyByDefault bool
	opts          []s3.Option
	domains       []string
}

type fixtureOption func(*fixtureConfig)

// asIdentity makes the requests come from id instead of root.
func asIdentity(id *domain.Identity) fixtureOption {
	return func(c *fixtureConfig) { c.identity = id }
}

// withoutIdentity leaves the requests without identity (no middleware).
func withoutIdentity() fixtureOption {
	return func(c *fixtureConfig) { c.noIdentity = true }
}

// withStrictPolicy registers no default "allow" on the policy mock: the
// test sets its own expectations.
func withStrictPolicy() fixtureOption {
	return func(c *fixtureConfig) { c.denyByDefault = true }
}

func withOptions(opts ...s3.Option) fixtureOption {
	return func(c *fixtureConfig) { c.opts = append(c.opts, opts...) }
}

func withDomains(domains ...string) fixtureOption {
	return func(c *fixtureConfig) { c.domains = domains }
}

func newFixture(t *testing.T, opts ...fixtureOption) *fixture {
	t.Helper()
	cfg := fixtureConfig{identity: &domain.Identity{AccessKey: "root", IsRoot: true}}
	for _, o := range opts {
		o(&cfg)
	}
	f := &fixture{
		t:        t,
		objects:  objectmock.NewService(t),
		buckets:  bucketmock.NewService(t),
		policy:   policymock.NewService(t),
		verifier: authmock.NewVerifier(t),
		ilm:      ilmmock.NewService(t),
		events:   eventmock.NewService(t),
		enc:      httpx.New(),
		clock:    clock.NewFake(now),
	}
	if !cfg.noIdentity {
		f.identity = cfg.identity
	}
	if !cfg.denyByDefault {
		f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Allowed: true}, nil).Maybe()
	}
	// The probe of the policies using s3:ExistingObjectTag finds none.
	f.policy.EXPECT().Simulate(mock.Anything, mock.Anything).Return(policy.Decision{}, nil).Maybe()
	api := s3.NewAPI(s3.APIDeps{Enc: f.enc, Domains: cfg.domains},
		s3.NewBucketRoutes(s3.BucketDeps{Buckets: f.buckets, Objects: f.objects, Policy: f.policy, Enc: f.enc, Clock: f.clock}, cfg.opts...),
		s3.NewObjectRoutes(s3.ObjectDeps{Objects: f.objects, Policy: f.policy, Enc: f.enc, Clock: f.clock}),
		s3.NewMultipartRoutes(s3.MultipartDeps{Objects: f.objects, Policy: f.policy, Enc: f.enc, Clock: f.clock}),
		s3.NewPresignedRoutes(s3.PresignedDeps{Verifier: f.verifier, Objects: f.objects, Buckets: f.buckets, Policy: f.policy, Enc: f.enc, Clock: f.clock}),
		s3.NewPostPolicyRoutes(s3.PostPolicyDeps{Verifier: f.verifier, Objects: f.objects, Policy: f.policy, Enc: f.enc, Clock: f.clock}),
		s3.NewLifecycleRoutes(s3.LifecycleDeps{Buckets: f.buckets, Lifecycle: f.ilm, Policy: f.policy, Enc: f.enc, Clock: f.clock}),
		s3.NewNotificationRoutes(s3.NotificationDeps{Buckets: f.buckets, Events: f.events, Policy: f.policy, Enc: f.enc, Clock: f.clock}),
	)
	r := chi.NewRouter()
	api.Register(r)
	f.handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if f.identity != nil {
			req = req.WithContext(middleware.WithIdentity(req.Context(), f.identity))
		}
		r.ServeHTTP(w, req)
	})
	return f
}

// request builds a request; a body (even empty) on PUT and POST gets its
// Content-Length header, as a real client sends it.
func request(method, target, body string, headers ...string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	if method == http.MethodPut || method == http.MethodPost {
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return req
}

func (f *fixture) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) do(method, target, body string, headers ...string) *httptest.ResponseRecorder {
	return f.serve(request(method, target, body, headers...))
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// s3Error decodes the XML error document of a response.
func s3Error(t *testing.T, rec *httptest.ResponseRecorder) httpx.S3ErrorResponse {
	t.Helper()
	var body httpx.S3ErrorResponse
	if rec.Body.Len() > 0 && strings.Contains(rec.Header().Get("Content-Type"), "xml") {
		require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	}
	return body
}

// requireError asserts the status and S3 code of an error response.
func requireError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	if code != "" {
		require.Equal(t, code, s3Error(t, rec).Code, rec.Body.String())
	}
}

// decodeXML decodes a response document.
func decodeXML(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	require.Equal(t, httpx.ContentTypeXML, rec.Header().Get("Content-Type"), rec.Body.String())
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), v), rec.Body.String())
}
