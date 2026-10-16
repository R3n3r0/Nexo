package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/audit"
	auditmock "github.com/r3n3r0/nexo/internal/service/audit/mock"
	"github.com/r3n3r0/nexo/internal/service/auth"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	metricsmock "github.com/r3n3r0/nexo/internal/service/metrics/mock"
	"github.com/r3n3r0/nexo/internal/service/session"
	sessionmock "github.com/r3n3r0/nexo/internal/service/session/mock"
	"github.com/r3n3r0/nexo/internal/testutil/redaction"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	httpxmock "github.com/r3n3r0/nexo/internal/transport/http/httpx/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	})
}

// fixedIDs returns the generated IDGenerator mock answering REQ / HOST.
func fixedIDs(t *testing.T) *httpxmock.IDGenerator {
	t.Helper()
	ids := httpxmock.NewIDGenerator(t)
	ids.EXPECT().NewRequestID().Return("REQ").Maybe()
	ids.EXPECT().NewHostID().Return("HOST").Maybe()
	return ids
}

func TestChainOrder(t *testing.T) {
	var order []string
	mk := func(name string) middleware.Middleware {
		return middleware.Func(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		})
	}
	h := middleware.Chain(okHandler(), mk("a"), nil, mk("b"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, []string{"a", "b"}, order)
	assert.Equal(t, http.StatusTeapot, rec.Code)
}

func TestRequestID(t *testing.T) {
	var got string
	h := middleware.NewRequestID(middleware.RequestIDDeps{IDs: fixedIDs(t)}).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = httpx.RequestIDFrom(r.Context())
		assert.Equal(t, "HOST", httpx.HostIDFrom(r.Context()))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, "REQ", got)
	assert.Equal(t, "REQ", rec.Header().Get("x-amz-request-id"))
	assert.Equal(t, "HOST", rec.Header().Get("x-amz-id-2"))

	// The default generator produces distinct ids.
	h = middleware.NewRequestID(middleware.RequestIDDeps{}).Wrap(okHandler())
	r1, r2 := httptest.NewRecorder(), httptest.NewRecorder()
	h.ServeHTTP(r1, httptest.NewRequest(http.MethodGet, "/", nil))
	h.ServeHTTP(r2, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.NotEqual(t, r1.Header().Get("x-amz-request-id"), r2.Header().Get("x-amz-request-id"))
}

func TestRecover(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })

	tests := []struct {
		name        string
		deps        middleware.RecoverDeps
		path        string
		contentType string
		body        string
	}{
		{name: "s3 flavor by default", deps: middleware.RecoverDeps{Logger: logger}, path: "/b/k", contentType: httpx.ContentTypeXML, body: "<Code>InternalError</Code>"},
		{name: "problem flavor", deps: middleware.RecoverDeps{Logger: logger, OnError: middleware.ProblemErrorWriter(httpx.New())}, path: "/api/v1/x", contentType: httpx.ContentTypeProblem, body: `"status":500`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			rec := httptest.NewRecorder()
			middleware.NewRecover(tt.deps).Wrap(panicking).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Contains(t, rec.Header().Get("Content-Type"), tt.contentType)
			assert.Contains(t, rec.Body.String(), tt.body)
			assert.Contains(t, buf.String(), "boom")
			assert.Contains(t, buf.String(), "stack")
		})
	}

	// ErrAbortHandler propagates; the default logger is used without one.
	abort := middleware.NewRecover(middleware.RecoverDeps{}).Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	assert.Panics(t, func() { abort.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil)) })
}

func TestLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clk.Advance(1500 * time.Microsecond)
		okHandler().ServeHTTP(w, r)
	})
	h := middleware.Chain(slow,
		middleware.NewRequestID(middleware.RequestIDDeps{IDs: fixedIDs(t)}),
		middleware.NewLogging(middleware.LoggingDeps{Logger: logger, Clock: clk}, middleware.WithLogLevel(slog.LevelInfo)))
	req := httptest.NewRequest(http.MethodPut, "/bucket/key?uploads", strings.NewReader("x"))
	req.RemoteAddr = "10.0.0.1:1234"
	h.ServeHTTP(httptest.NewRecorder(), req)

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "PUT", rec["method"])
	assert.Equal(t, "/bucket/key", rec["path"])
	assert.Equal(t, "uploads", rec["query"])
	assert.Equal(t, float64(http.StatusTeapot), rec["status"])
	assert.Equal(t, float64(5), rec["bytes"])
	assert.Equal(t, "REQ", rec["request_id"])
	assert.Equal(t, "10.0.0.1:1234", rec["remote"])
	assert.Equal(t, float64(1500*time.Microsecond), rec["duration"])

	// Presigned signatures, credentials and tokens never reach the log.
	buf.Reset()
	req = httptest.NewRequest(http.MethodGet, "/bucket/key?X-Amz-Credential=AKIA%2Fx&X-Amz-Signature=f8de&X-Amz-Expires=60", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	rec = nil
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "X-Amz-Credential=REDACTED&X-Amz-Signature=REDACTED&X-Amz-Expires=60", rec["query"])
	assert.NotContains(t, buf.String(), "f8de")

	// The table every redaction shares (regression: token and password
	// values reached the stderr log).
	buf.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/bucket/key?"+redaction.Query, nil))
	assert.Empty(t, redaction.Leaks(buf.String()))
	for _, kept := range redaction.Kept() {
		assert.Contains(t, buf.String(), kept)
	}

	// Below the configured level nothing is written; the defaults apply
	// without a logger or a clock.
	buf.Reset()
	middleware.NewLogging(middleware.LoggingDeps{Logger: logger}, middleware.WithLogLevel(slog.LevelDebug)).Wrap(okHandler()).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Empty(t, buf.String())
	assert.NotNil(t, middleware.NewLogging(middleware.LoggingDeps{}))
}

// TestLoggingRecordsTheErrorChain: the problem documents carry a curated
// detail, so the chain of the error must reach the log of the request,
// with its request id; a server failure is logged above the debug level
// of the access log.
func TestLoggingRecordsTheErrorChain(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	enc := httpx.New()
	failing := func(err error) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { enc.WriteProblemError(w, r, err) })
	}
	tests := []struct {
		name   string
		err    error
		status int
		level  string
	}{
		{"quorum lost", fmt.Errorf("bucket: create q: erasure: lock q: %w: resource \"q\" after 30s", domain.ErrLockTimeout), http.StatusServiceUnavailable, "WARN"},
		{"internal", errors.New("fs: rename /data/d1/tmp: input/output error"), http.StatusInternalServerError, "ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			h := middleware.Chain(failing(tt.err),
				middleware.NewRequestID(middleware.RequestIDDeps{IDs: fixedIDs(t)}),
				middleware.NewLogging(middleware.LoggingDeps{Logger: logger}, middleware.WithLogLevel(slog.LevelDebug)))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/buckets", nil))
			assert.Equal(t, tt.status, rec.Code)
			assert.NotContains(t, rec.Body.String(), tt.err.Error(), "the chain stays out of the response")
			var line map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &line), buf.String())
			assert.Equal(t, tt.err.Error(), line["error"])
			assert.Equal(t, "REQ", line["request_id"])
			assert.Equal(t, tt.level, line["level"])
		})
	}

	// A client error keeps the level of the access log.
	buf.Reset()
	middleware.NewLogging(middleware.LoggingDeps{Logger: logger}, middleware.WithLogLevel(slog.LevelDebug)).
		Wrap(failing(fmt.Errorf("x: %w", domain.ErrBucketNotFound))).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/buckets/nope", nil))
	assert.Empty(t, buf.String())
}

func TestCORS(t *testing.T) {
	preflight := func(h http.Handler, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodOptions, "/b", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "PUT")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	get := func(h http.Handler, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/b", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	open := middleware.NewCORS(middleware.CORSDeps{}).Wrap(okHandler())
	rec := preflight(open, "https://app.example.com")
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "PUT")
	assert.Equal(t, "300", rec.Header().Get("Access-Control-Max-Age"))

	restricted := middleware.NewCORS(middleware.CORSDeps{},
		middleware.WithAllowedOrigins("https://ok.example.com"),
		middleware.WithAllowedMethods(http.MethodGet, http.MethodPut),
		middleware.WithAllowedHeaders("Authorization"),
		middleware.WithExposedHeaders("ETag"),
		middleware.WithAllowCredentials(true),
		middleware.WithMaxAge(60),
	).Wrap(okHandler())
	assert.Empty(t, get(restricted, "https://evil.example.com").Header().Get("Access-Control-Allow-Origin"))
	rec = get(restricted, "https://ok.example.com")
	assert.Equal(t, "https://ok.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", rec.Header().Get("Access-Control-Allow-Credentials"))
	assert.True(t, strings.EqualFold("ETag", rec.Header().Get("Access-Control-Expose-Headers")))
	assert.Equal(t, "60", preflight(restricted, "https://ok.example.com").Header().Get("Access-Control-Max-Age"))
}

// metricsMocks returns a registry mock handing out instrument mocks.
func metricsMocks(t *testing.T) (*metricsmock.Registry, *metricsmock.Counter, *metricsmock.Histogram, *metricsmock.Gauge) {
	t.Helper()
	reg := metricsmock.NewRegistry(t)
	requests := metricsmock.NewCounter(t)
	duration := metricsmock.NewHistogram(t)
	inflight := metricsmock.NewGauge(t)
	reg.EXPECT().Counter(mock.Anything, mock.MatchedBy(func(d metrics.Definition) bool {
		return d.Subsystem == "http" && d.Name == "requests_total" && assert.ObjectsAreEqual([]string{"server", "method", "status"}, d.Labels)
	})).Return(requests, nil).Maybe()
	reg.EXPECT().Histogram(mock.Anything, mock.MatchedBy(func(d metrics.Definition) bool {
		return d.Name == "request_duration_seconds" && assert.ObjectsAreEqual([]string{"server", "method"}, d.Labels)
	})).Return(duration, nil).Maybe()
	reg.EXPECT().Gauge(mock.Anything, mock.MatchedBy(func(d metrics.Definition) bool {
		return d.Name == "requests_in_flight" && assert.ObjectsAreEqual([]string{"server"}, d.Labels)
	})).Return(inflight, nil).Maybe()
	return reg, requests, duration, inflight
}

func TestMetrics(t *testing.T) {
	reg, requests, duration, inflight := metricsMocks(t)
	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	m, err := middleware.NewMetrics(middleware.MetricsDeps{Registry: reg, Clock: clk}, middleware.WithMetricsServer("s3"))
	require.NoError(t, err)

	inflight.EXPECT().Add(mock.Anything, float64(1), "s3").Return().Times(2)
	inflight.EXPECT().Add(mock.Anything, float64(-1), "s3").Return().Times(2)
	requests.EXPECT().Add(mock.Anything, float64(1), "s3", "GET", "418").Return().Once()
	requests.EXPECT().Add(mock.Anything, float64(1), "s3", "OTHER", "418").Return().Once()
	duration.EXPECT().Observe(mock.Anything, 0.25, "s3", "GET").Return().Once()
	duration.EXPECT().Observe(mock.Anything, 0.25, "s3", "OTHER").Return().Once()

	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clk.Advance(250 * time.Millisecond)
		okHandler().ServeHTTP(w, r)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PROPFIND", "/", nil))
}

func TestMetricsConstruction(t *testing.T) {
	_, err := middleware.NewMetrics(middleware.MetricsDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "a registry is required")

	failing := func(kind string) *metricsmock.Registry {
		reg := metricsmock.NewRegistry(t)
		clash := fmt.Errorf("%w: clash", domain.ErrInvalidArgument)
		okCounter, okHist := metricsmock.NewCounter(t), metricsmock.NewHistogram(t)
		switch kind {
		case "counter":
			reg.EXPECT().Counter(mock.Anything, mock.Anything).Return(nil, clash).Once()
		case "histogram":
			reg.EXPECT().Counter(mock.Anything, mock.Anything).Return(okCounter, nil).Once()
			reg.EXPECT().Histogram(mock.Anything, mock.Anything).Return(nil, clash).Once()
		case "gauge":
			reg.EXPECT().Counter(mock.Anything, mock.Anything).Return(okCounter, nil).Once()
			reg.EXPECT().Histogram(mock.Anything, mock.Anything).Return(okHist, nil).Once()
			reg.EXPECT().Gauge(mock.Anything, mock.Anything).Return(nil, clash).Once()
		}
		return reg
	}
	for _, kind := range []string{"counter", "histogram", "gauge"} {
		t.Run(kind, func(t *testing.T) {
			_, err := middleware.NewMetrics(middleware.MetricsDeps{Registry: failing(kind)})
			assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}

	// The real registry: two listeners share the definitions.
	reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
	for _, server := range []string{"s3", "console"} {
		m, err := middleware.NewMetrics(middleware.MetricsDeps{Registry: reg}, middleware.WithMetricsServer(server))
		require.NoError(t, err)
		m.Wrap(okHandler()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Contains(t, rec.Body.String(), `nexo_http_requests_total{method="GET",server="s3",status="418"} 1`)
	assert.Contains(t, rec.Body.String(), `nexo_http_requests_total{method="GET",server="console",status="418"} 1`)
}

func TestAuth(t *testing.T) {
	_, err := middleware.NewAuth(middleware.AuthDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "a verifier is required")

	var got *domain.Identity
	var ok bool
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = middleware.IdentityFromContext(r.Context())
	})
	v := authmock.NewVerifier(t)
	m, err := middleware.NewAuth(middleware.AuthDeps{Verifier: v})
	require.NoError(t, err)
	h := m.Wrap(capture)

	tests := []struct {
		name       string
		identity   *domain.Identity
		err        error
		wantStatus int
		wantKey    string
		anonymous  bool
	}{
		{name: "verified identity", identity: &domain.Identity{AccessKey: "alice"}, wantStatus: http.StatusOK, wantKey: "alice"},
		{name: "nil identity is anonymous", wantStatus: http.StatusOK, anonymous: true},
		{name: "failure writes the S3 error", err: fmt.Errorf("sig: %w", domain.ErrSignatureMismatch), wantStatus: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok = nil, false
			v.EXPECT().Verify(mock.Anything, mock.Anything).Return(tt.identity, tt.err).Once()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/b", nil))
			assert.Equal(t, tt.wantStatus, rec.Code)
			if tt.err != nil {
				assert.Contains(t, rec.Body.String(), "SignatureDoesNotMatch")
				assert.False(t, ok, "the handler is not reached")
				return
			}
			require.True(t, ok)
			assert.Equal(t, tt.anonymous, got.IsAnonymous)
			assert.Equal(t, tt.wantKey, got.AccessKey)
		})
	}

	// Without middleware the context has no identity.
	_, ok = middleware.IdentityFromContext(context.Background())
	assert.False(t, ok)

	// Skipped paths authenticate themselves: the verifier is not called
	// (the mock has no expectation left) and no identity is set.
	skip, err := middleware.NewAuth(middleware.AuthDeps{Verifier: v, Skip: func(r *http.Request) bool {
		return strings.HasPrefix(r.URL.Path, "/nexo/metrics/")
	}})
	require.NoError(t, err)
	got, ok = nil, false
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/nexo/metrics/v1/node", nil)
	req.Header.Set("Authorization", "Bearer token")
	skip.Wrap(capture).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, ok)
	v.EXPECT().Verify(mock.Anything, mock.Anything).Return(&domain.Identity{AccessKey: "bob"}, nil).Once()
	skip.Wrap(capture).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nexo/metricsx", nil))
	require.True(t, ok, "only the prefix is skipped")
	assert.Equal(t, "bob", got.AccessKey)
}

// The expected SigV4 service of a request reaches the verifier (and the
// handlers) through the context.
func TestAuthExpectedService(t *testing.T) {
	v := authmock.NewVerifier(t)
	m, err := middleware.NewAuth(middleware.AuthDeps{Verifier: v, Service: func(r *http.Request) string {
		if r.Method == http.MethodPost {
			return auth.ServiceSTS
		}
		return auth.ServiceS3
	}})
	require.NoError(t, err)
	var seen []string
	v.EXPECT().Verify(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ *http.Request) (*domain.Identity, error) {
		seen = append(seen, auth.ExpectedService(ctx))
		return domain.Anonymous(), nil
	}).Times(2)
	var downstream string
	h := m.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { downstream = auth.ExpectedService(r.Context()) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	assert.Equal(t, auth.ServiceSTS, downstream)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/b/k", nil))
	assert.Equal(t, []string{auth.ServiceSTS, auth.ServiceS3}, seen)
}

func TestAudit(t *testing.T) {
	_, err := middleware.NewAudit(middleware.AuditDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "an auditor is required")

	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	auditor := auditmock.NewService(t)
	m, err := middleware.NewAudit(middleware.AuditDeps{Auditor: auditor, Clock: clk})
	require.NoError(t, err)

	tests := []struct {
		name      string
		target    string
		operation string
		identity  *domain.Identity
		// header is the Authorization header of the request; status the
		// answer of the handler (default okHandler's).
		header string
		status int
		check  func(t *testing.T, e audit.Entry)
	}{
		{
			name: "named operation with identity", target: "/photos/2026/a.jpg?versionId=1", operation: "GetObject",
			identity: &domain.Identity{AccessKey: "AKIA1", Parent: "bob"},
			check: func(t *testing.T, e audit.Entry) {
				assert.Equal(t, "GetObject", e.API.Name)
				assert.Equal(t, "photos", e.API.Bucket)
				assert.Equal(t, "2026/a.jpg", e.API.Object)
				assert.Equal(t, http.StatusTeapot, e.API.StatusCode)
				assert.Equal(t, int64(5), e.API.OutputBytes)
				assert.Equal(t, 2*time.Millisecond, e.API.TimeToResponse)
				assert.Equal(t, "AKIA1", e.AccessKey)
				assert.Equal(t, "bob", e.ParentUser)
				assert.Equal(t, "10.0.0.1", e.RemoteHost)
				assert.Equal(t, map[string]string{"versionId": "1"}, e.ReqQuery)
				assert.Equal(t, audit.EntryVersion, e.Version)
			},
		},
		{
			name: "presigned capability redacted", operation: "PresignedGetObject",
			target: "/photos/a.jpg?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIA1%2F20261008%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Expires=300&X-Amz-Security-Token=tok&X-Amz-Signature=f8de",
			check: func(t *testing.T, e audit.Entry) {
				assert.Equal(t, map[string]string{
					"X-Amz-Algorithm": "AWS4-HMAC-SHA256", "X-Amz-Expires": "300",
					"X-Amz-Credential": httpx.RedactedValue, "X-Amz-Security-Token": httpx.RedactedValue, "X-Amz-Signature": httpx.RedactedValue,
				}, e.ReqQuery)
			},
		},
		{
			name: "unnamed operation", target: "/",
			check: func(t *testing.T, e audit.Entry) {
				assert.Equal(t, "GET /", e.API.Name)
				assert.Empty(t, e.API.Bucket)
				assert.Empty(t, e.AccessKey)
				assert.Nil(t, e.ReqQuery)
			},
		},
		{
			name: "secrets of every kind redacted", target: "/photos/a.jpg?" + redaction.Query,
			check: func(t *testing.T, e audit.Entry) {
				raw, err := json.Marshal(e)
				require.NoError(t, err)
				assert.Empty(t, redaction.Leaks(string(raw)))
				assert.Equal(t, "10", e.ReqQuery["max-keys"])
			},
		},
		// The anonymous probes are not audited (nil check): they would push
		// the requests of the users out of the ring.
		{name: "health probe", target: "/nexo/health/ready", status: http.StatusOK},
		{name: "health probe of a node not ready", target: "/nexo/health/ready", status: http.StatusServiceUnavailable},
		{name: "anonymous metrics scrape", target: "/nexo/metrics/v1/node", status: http.StatusOK},
		{
			name: "authenticated metrics scrape", target: "/nexo/metrics/v1/node", identity: &domain.Identity{AccessKey: "AKIA1"},
			header: "Bearer token", status: http.StatusOK,
			check: func(t *testing.T, e audit.Entry) { assert.Equal(t, "AKIA1", e.AccessKey) },
		},
		// An attempt to authenticate, or a refused scrape, is recorded
		// (regression: a signed request to a health path, or a forged
		// bearer token, tested credentials without an audit trail).
		{
			name: "signed health request refused", target: "/nexo/health/live", status: http.StatusForbidden,
			header: "AWS4-HMAC-SHA256 Credential=AKIA1/20261008/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00",
			check: func(t *testing.T, e audit.Entry) {
				assert.Equal(t, "/nexo/health/live", e.API.Path)
				assert.Equal(t, http.StatusForbidden, e.API.StatusCode)
			},
		},
		{
			name: "signed health request served", target: "/nexo/health/live", status: http.StatusOK, identity: &domain.Identity{AccessKey: "AKIA1"},
			header: "AWS4-HMAC-SHA256 Credential=AKIA1/20261008/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00",
			check:  func(t *testing.T, e audit.Entry) { assert.Equal(t, "AKIA1", e.AccessKey) },
		},
		{
			name: "presigned health request", target: "/nexo/health/live?X-Amz-Credential=AKIA1&X-Amz-Signature=00", status: http.StatusForbidden,
			check: func(t *testing.T, e audit.Entry) { assert.Equal(t, http.StatusForbidden, e.API.StatusCode) },
		},
		{
			name: "presigned V2 health request", target: "/nexo/health/live?AWSAccessKeyId=AKIA1&Signature=00&Expires=1", status: http.StatusForbidden,
			check: func(t *testing.T, e audit.Entry) { assert.Equal(t, "/nexo/health/live", e.API.Path) },
		},
		{
			name: "metrics scrape with a forged token", target: "/nexo/metrics/v1/node", header: "Bearer garbage", status: http.StatusForbidden,
			check: func(t *testing.T, e audit.Entry) { assert.Equal(t, http.StatusForbidden, e.API.StatusCode) },
		},
		{
			name: "metrics scrape without token refused", target: "/nexo/metrics/v1/cluster", status: http.StatusForbidden,
			check: func(t *testing.T, e audit.Entry) { assert.Equal(t, "/nexo/metrics/v1/cluster", e.API.Path) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.check != nil {
				auditor.EXPECT().Log(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, e audit.Entry) error {
					tt.check(t, e)
					return errors.New("sink down: the request is not affected")
				}).Once()
			}
			h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.operation != "" {
					httpx.SetOperationName(r.Context(), tt.operation)
				}
				clk.Advance(2 * time.Millisecond)
				if tt.status != 0 {
					w.WriteHeader(tt.status)
					return
				}
				okHandler().ServeHTTP(w, r)
			}))
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			req.RemoteAddr = "10.0.0.1:5555"
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			if tt.identity != nil {
				req = req.WithContext(middleware.WithIdentity(req.Context(), tt.identity))
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			want := tt.status
			if want == 0 {
				want = http.StatusTeapot
			}
			assert.Equal(t, want, rec.Code)
		})
	}
}

// TestAuditWrapsAuthentication is a regression test: audit runs outside
// the auth middleware and learns the identity through its holder, so that
// requests rejected by the authentication are recorded, and an identity a
// handler establishes itself (POST policy, presigned URLs) on a derived
// context is the one recorded.
func TestAuditWrapsAuthentication(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	auditor := auditmock.NewService(t)
	verifier := authmock.NewVerifier(t)
	auditing, err := middleware.NewAudit(middleware.AuditDeps{Auditor: auditor, Clock: clk})
	require.NoError(t, err)
	authn, err := middleware.NewAuth(middleware.AuthDeps{Verifier: verifier})
	require.NoError(t, err)
	signer := &domain.Identity{AccessKey: "POLICYSIGNER", Parent: "carol"}

	tests := []struct {
		name       string
		verify     func(*authmock.Verifier)
		handler    http.HandlerFunc
		wantStatus int
		wantKey    string
		wantParent string
	}{
		{
			name: "rejected by the authentication",
			verify: func(v *authmock.Verifier) {
				v.EXPECT().Verify(mock.Anything, mock.Anything).Return(nil, domain.ErrSignatureMismatch).Once()
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "authenticated by the middleware",
			verify: func(v *authmock.Verifier) {
				v.EXPECT().Verify(mock.Anything, mock.Anything).Return(&domain.Identity{AccessKey: "AKIA1", Parent: "bob"}, nil).Once()
			},
			handler:    func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			wantStatus: http.StatusOK, wantKey: "AKIA1", wantParent: "bob",
		},
		{
			name: "authenticated by the handler (POST policy)",
			verify: func(v *authmock.Verifier) {
				v.EXPECT().Verify(mock.Anything, mock.Anything).Return(domain.Anonymous(), nil).Once()
			},
			handler: func(w http.ResponseWriter, r *http.Request) {
				r = r.WithContext(middleware.WithIdentity(r.Context(), signer))
				httpx.SetOperationName(r.Context(), "PostObject")
				w.WriteHeader(http.StatusNoContent)
			},
			wantStatus: http.StatusNoContent, wantKey: "POLICYSIGNER", wantParent: "carol",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.verify(verifier)
			var got audit.Entry
			auditor.EXPECT().Log(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, e audit.Entry) error {
				got = e
				return nil
			}).Once()
			handler := http.Handler(tt.handler)
			if tt.handler == nil {
				handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("the handler is not reached") })
			}
			rec := httptest.NewRecorder()
			middleware.Chain(handler, auditing, authn).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/bkt/key", nil))
			require.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantStatus, got.API.StatusCode, "the request is recorded")
			assert.Equal(t, "bkt", got.API.Bucket)
			assert.Equal(t, tt.wantKey, got.AccessKey)
			assert.Equal(t, tt.wantParent, got.ParentUser)
		})
	}

	// Without holder (no audit) the identity is only in the context.
	_, ok := middleware.HeldIdentity(middleware.WithIdentity(context.Background(), signer))
	assert.False(t, ok)
	ctx := middleware.WithIdentityHolder(context.Background())
	_, ok = middleware.HeldIdentity(ctx)
	assert.False(t, ok, "an empty holder holds no identity")
}

func TestSessionAuth(t *testing.T) {
	_, err := middleware.NewSessionAuth(middleware.SessionAuthDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "a session service is required")

	sessions := sessionmock.NewService(t)
	m, err := middleware.NewSessionAuth(middleware.SessionAuthDeps{Sessions: sessions})
	require.NoError(t, err)
	sess := &session.Session{ID: "s1", AccessKey: "bob", Identity: domain.Identity{AccessKey: "bob"}, CSRFToken: "csrf"}
	var seen *domain.Identity
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = middleware.IdentityFromContext(r.Context())
		assert.Same(t, sess, middleware.SessionFromContext(r.Context()))
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name       string
		method     string
		cookie     string
		csrf       string
		setup      func()
		wantStatus int
		wantCode   string
	}{
		{name: "no cookie", method: http.MethodGet, wantStatus: http.StatusUnauthorized, wantCode: "unauthenticated"},
		{name: "invalid session", method: http.MethodGet, cookie: "bad", setup: func() {
			sessions.EXPECT().Validate(mock.Anything, "bad").Return(nil, domain.ErrAccessDenied).Once()
		}, wantStatus: http.StatusUnauthorized, wantCode: "unauthenticated"},
		{name: "read without csrf", method: http.MethodGet, cookie: "tok", setup: func() {
			sessions.EXPECT().Validate(mock.Anything, "tok").Return(sess, nil).Once()
		}, wantStatus: http.StatusNoContent},
		{name: "write without csrf", method: http.MethodPost, cookie: "tok", setup: func() {
			sessions.EXPECT().Validate(mock.Anything, "tok").Return(sess, nil).Once()
			sessions.EXPECT().ValidateCSRF(mock.Anything, sess, "").Return(domain.ErrAccessDenied).Once()
		}, wantStatus: http.StatusForbidden, wantCode: "invalid_csrf_token"},
		{name: "write with csrf", method: http.MethodDelete, cookie: "tok", csrf: "csrf", setup: func() {
			sessions.EXPECT().Validate(mock.Anything, "tok").Return(sess, nil).Once()
			sessions.EXPECT().ValidateCSRF(mock.Anything, sess, "csrf").Return(nil).Once()
		}, wantStatus: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen = nil
			if tt.setup != nil {
				tt.setup()
			}
			req := httptest.NewRequest(tt.method, "/api/v1/users", nil)
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: session.CookieName, Value: tt.cookie})
			}
			if tt.csrf != "" {
				req.Header.Set(session.CSRFHeader, tt.csrf)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, tt.wantStatus, rec.Code)
			if tt.wantCode != "" {
				var p httpx.Problem
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
				assert.Equal(t, tt.wantCode, p.Code)
				assert.Nil(t, seen)
				return
			}
			require.NotNil(t, seen)
			assert.Equal(t, "bob", seen.AccessKey)
		})
	}
}

func TestMaxBody(t *testing.T) {
	var readErr error
	h := middleware.NewMaxBody(middleware.MaxBodyDeps{}, middleware.WithMaxBodyLimit(4)).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		if readErr != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/", strings.NewReader("toolong")))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	var mbe *http.MaxBytesError
	assert.True(t, errors.As(readErr, &mbe))

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/", strings.NewReader("ok")))
	assert.Equal(t, http.StatusOK, rec.Code)

	// A disabled limit returns the handler unchanged; the default is 64 MiB.
	passthrough := okHandler()
	assert.NotNil(t, middleware.NewMaxBody(middleware.MaxBodyDeps{}, middleware.WithMaxBodyLimit(0)).Wrap(passthrough))
	rec = httptest.NewRecorder()
	middleware.NewMaxBody(middleware.MaxBodyDeps{}).Wrap(passthrough).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/", strings.NewReader("small")))
	assert.Equal(t, http.StatusTeapot, rec.Code)
}
