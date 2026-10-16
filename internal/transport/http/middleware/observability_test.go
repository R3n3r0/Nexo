package middleware_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/trace"
	tracemock "github.com/r3n3r0/nexo/internal/service/trace/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	httpxmock "github.com/r3n3r0/nexo/internal/transport/http/httpx/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// s3Handler names the operation, authenticates the caller (below the
// observability middlewares, like the SigV4 middleware) and answers.
func s3Handler(op string, id *domain.Identity, status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.SetOperationName(r.Context(), op)
		if id != nil {
			middleware.WithIdentity(r.Context(), id)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

// secretRequest is a presigned PUT carrying every kind of secret.
func secretRequest() *http.Request {
	q := "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIDSECRET%2F20260101%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Signature=deadbeefsignature&X-Amz-Security-Token=stssessiontoken&versionId=v1"
	r := httptest.NewRequest(http.MethodPut, "/photos/a/b.jpg?"+q, strings.NewReader("hello"))
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDHEADER/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=headersignature")
	r.Header.Set("X-Amz-Server-Side-Encryption-Customer-Key", "c3NlY2N1c3RvbWVya2V5")
	r.Header.Set("Cookie", "nexo_session=sessioncookie")
	r.RemoteAddr = "10.0.0.7:5555"
	return r
}

// secrets are the values secretRequest carries.
var secrets = []string{"AKIDSECRET", "deadbeefsignature", "stssessiontoken", "AKIDHEADER", "headersignature", "c3NlY2N1c3RvbWVya2V5", "sessioncookie"}

func TestTraceMiddleware(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracer, err := trace.New(trace.Deps{NodeID: "n1"})
	require.NoError(t, err)
	sub, err := tracer.Subscribe(ctx, trace.Filter{})
	require.NoError(t, err)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	mw, err := middleware.NewTrace(middleware.TraceDeps{Tracer: tracer, Clock: clk})
	require.NoError(t, err)
	reqID := middleware.NewRequestID(middleware.RequestIDDeps{IDs: fixedIDs(t)})
	h := middleware.Chain(s3Handler("PutObject", &domain.Identity{AccessKey: "alice"}, http.StatusOK, "done"), reqID, mw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, secretRequest())
	require.Equal(t, http.StatusOK, rec.Code)

	var e trace.Event
	select {
	case e = <-sub.Events():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no trace event")
	}
	assert.Equal(t, "n1", e.Node)
	assert.Equal(t, trace.TypeS3, e.Type)
	assert.Equal(t, "PutObject", e.API)
	assert.Equal(t, http.MethodPut, e.Method)
	assert.Equal(t, "/photos/a/b.jpg", e.Path)
	assert.Equal(t, http.StatusOK, e.Status)
	assert.Equal(t, int64(5), e.InputBytes)
	assert.Equal(t, int64(4), e.OutputBytes)
	assert.Equal(t, "REQ", e.RequestID)
	assert.Equal(t, "10.0.0.7", e.ClientIP)
	assert.Equal(t, "alice", e.User)
	assert.Equal(t, "photos", e.Bucket)
	assert.Empty(t, e.Error)
	assert.Contains(t, e.Query, "versionId=v1")
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	for _, s := range secrets {
		assert.NotContains(t, string(raw), s, "a secret reached the trace")
	}
	assert.NotContains(t, string(raw), "Authorization")
}

func TestTraceMiddlewareIdleAndScope(t *testing.T) {
	_, err := middleware.NewTrace(middleware.TraceDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	// Nobody subscribes: the requests are not observed at all.
	idle := tracemock.NewService(t)
	idle.EXPECT().Enabled().Return(false)
	mw, err := middleware.NewTrace(middleware.TraceDeps{Tracer: idle})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	middleware.Chain(okHandler(), mw).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/b/k", nil))
	assert.Equal(t, http.StatusTeapot, rec.Code)

	// The console: admin type, bucket from the Admin API path, only the
	// requests selected, the API named by the fallback.
	busy := tracemock.NewService(t)
	busy.EXPECT().Enabled().Return(true)
	busy.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(e trace.Event) bool {
		return e.Type == trace.TypeAdmin && e.Bucket == "photos" && e.API == "GET /api/v1/buckets/photos/objects" &&
			e.Status == http.StatusTeapot && e.Error == http.StatusText(http.StatusTeapot)
	})).Once()
	mw, err = middleware.NewTrace(middleware.TraceDeps{Tracer: busy}, middleware.WithTraceType(trace.TypeAdmin),
		middleware.WithTraceOnly(func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/") }),
		middleware.WithTraceAPI(func(r *http.Request) string { return r.Method + " " + r.URL.Path }))
	require.NoError(t, err)
	h := middleware.Chain(okHandler(), mw)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/buckets/photos/objects", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
}

func TestTraceMiddlewareVirtualHost(t *testing.T) {
	tracer := tracemock.NewService(t)
	tracer.EXPECT().Enabled().Return(true)
	tracer.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(e trace.Event) bool { return e.Bucket == "photos" && e.Path == "/a.jpg" })).Once()
	mw, err := middleware.NewTrace(middleware.TraceDeps{Tracer: tracer}, middleware.WithTraceDomains("s3.example.com"))
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodGet, "/a.jpg", nil)
	r.Host = "photos.s3.example.com:9000"
	middleware.Chain(okHandler(), mw).ServeHTTP(httptest.NewRecorder(), r)
}

func TestAPIMetrics(t *testing.T) {
	_, err := middleware.NewAPIMetrics(middleware.APIMetricsDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	ctx := context.Background()
	reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
	mw, err := middleware.NewAPIMetrics(middleware.APIMetricsDeps{Registry: reg}, middleware.WithBucketLabelLimit(2))
	require.NoError(t, err)
	alice := &domain.Identity{AccessKey: "alice"}
	serve := func(method, target, op string, id *domain.Identity, status int, body string) {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		middleware.Chain(s3Handler(op, id, status, "out"), mw).ServeHTTP(httptest.NewRecorder(), r)
	}
	serve(http.MethodPut, "/photos/a.jpg", "PutObject", alice, http.StatusOK, "12345")
	serve(http.MethodPut, "/photos/b.jpg", "PutObject", alice, http.StatusOK, "123")
	serve(http.MethodGet, "/docs/x", "GetObject", alice, http.StatusForbidden, "")
	serve(http.MethodGet, "/random1/x", "GetObject", nil, http.StatusNotFound, "")   // anonymous probe: no bucket label
	serve(http.MethodGet, "/random2/x", "GetObject", alice, http.StatusNotFound, "") // 404: no bucket label
	serve(http.MethodGet, "/backup/x", "GetObject", nil, http.StatusInternalServerError, "")
	serve(http.MethodGet, "/third/x", "GetObject", alice, http.StatusOK, "") // beyond the limit
	serve(http.MethodGet, "/", "", nil, http.StatusForbidden, "")

	fams, err := reg.Snapshot(ctx)
	require.NoError(t, err)
	value := func(name string, labels map[string]string) float64 {
		t.Helper()
		for _, f := range fams {
			if f.Name != name {
				continue
			}
			for _, s := range f.Samples {
				if assert.ObjectsAreEqual(labels, s.Labels) || (len(labels) == 0 && len(s.Labels) == 0) {
					return s.Value
				}
			}
		}
		require.Failf(t, "sample missing", "%s%v", name, labels)
		return 0
	}
	assert.InDelta(t, 2, value("nexo_s3_requests_total", map[string]string{"api": "PutObject", "status": "200"}), 0)
	assert.InDelta(t, 1, value("nexo_s3_requests_total", map[string]string{"api": "unknown", "status": "403"}), 0)
	assert.InDelta(t, 3, value("nexo_s3_requests_4xx_errors_total", map[string]string{"api": "GetObject"}), 0)
	assert.InDelta(t, 1, value("nexo_s3_requests_5xx_errors_total", map[string]string{"api": "GetObject"}), 0)
	assert.InDelta(t, 8, value("nexo_s3_traffic_received_bytes_total", nil), 0)
	assert.InDelta(t, 3*8, value("nexo_s3_traffic_sent_bytes_total", nil), 0)
	assert.InDelta(t, 0, value("nexo_s3_requests_inflight", nil), 0)
	assert.InDelta(t, 2, value("nexo_bucket_requests_total", map[string]string{"bucket": "photos", "api": "PutObject", "status_class": "2xx"}), 0)
	assert.InDelta(t, 8, value("nexo_bucket_traffic_received_bytes_total", map[string]string{"bucket": "photos"}), 0)
	assert.InDelta(t, 1, value("nexo_bucket_requests_total", map[string]string{"bucket": "docs", "api": "GetObject", "status_class": "4xx"}), 0)
	assert.InDelta(t, 1, value("nexo_bucket_requests_total", map[string]string{"bucket": "_other", "api": "GetObject", "status_class": "5xx"}), 0)
	assert.InDelta(t, 1, value("nexo_bucket_requests_total", map[string]string{"bucket": "_other", "api": "GetObject", "status_class": "2xx"}), 0)
	for _, f := range fams {
		for _, s := range f.Samples {
			for _, v := range s.Labels {
				assert.NotContains(t, []string{"random1", "random2", "a.jpg", "x"}, v, "%s: unbounded label", f.Name)
			}
		}
	}
}

func TestTracingMiddleware(t *testing.T) {
	_, err := middleware.NewTracing(middleware.TracingDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	// Disabled: the middleware is the next handler itself.
	off := tracemock.NewTelemetry(t)
	off.EXPECT().Enabled().Return(false)
	mw, err := middleware.NewTracing(middleware.TracingDeps{Telemetry: off})
	require.NoError(t, err)
	next := okHandler()
	assert.Equal(t, reflect.ValueOf(next).Pointer(), reflect.ValueOf(mw.Wrap(next)).Pointer(), "zero overhead when disabled")

	tel := tracemock.NewTelemetry(t)
	span := tracemock.NewSpan(t)
	type ctxKey struct{}
	tel.EXPECT().Enabled().Return(true)
	tel.EXPECT().Extract(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, h http.Header) context.Context {
		assert.Equal(t, "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", h.Get("Traceparent"))
		return context.WithValue(ctx, ctxKey{}, "parent")
	})
	var attrs []trace.Attribute
	tel.EXPECT().StartSpan(mock.Anything, "s3 PUT", mock.Anything).RunAndReturn(func(ctx context.Context, _ string, o trace.SpanOptions) (context.Context, trace.Span) {
		assert.Equal(t, "parent", ctx.Value(ctxKey{}), "the span continues the trace of the caller")
		assert.Equal(t, trace.SpanServer, o.Kind)
		attrs = append(attrs, o.Attributes...)
		return ctx, span
	})
	span.EXPECT().SetName("s3 PutObject").Once()
	span.EXPECT().SetAttributes(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(a ...trace.Attribute) { attrs = append(attrs, a...) }).Once()
	span.EXPECT().SetError(http.StatusText(http.StatusInternalServerError)).Once()
	span.EXPECT().End().Once()
	mw, err = middleware.NewTracing(middleware.TracingDeps{Telemetry: tel}, middleware.WithTracingServer("s3"))
	require.NoError(t, err)
	r := secretRequest()
	r.Header.Set("Traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	middleware.Chain(s3Handler("PutObject", &domain.Identity{AccessKey: "alice"}, http.StatusInternalServerError, ""), mw).
		ServeHTTP(httptest.NewRecorder(), r)

	got := map[string]any{}
	for _, a := range attrs {
		got[a.Key] = a.Value
	}
	assert.Equal(t, "/photos/a/b.jpg", got["url.path"], "the path, never the query")
	assert.Equal(t, http.StatusInternalServerError, got["http.response.status_code"])
	assert.Equal(t, "PutObject", got["nexo.api"])
	assert.Equal(t, "photos", got["nexo.bucket"])
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	for _, s := range secrets {
		assert.NotContains(t, string(raw), s, "a secret reached a span")
	}
}

// TestAuditSharesHolders checks that the observability middlewares share
// the operation and identity of a request, whatever their order, and that
// the audit record carries no secret.
func TestAuditSharesHolders(t *testing.T) {
	auditor := auditmock.NewService(t)
	var entry audit.Entry
	auditor.EXPECT().Log(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, e audit.Entry) error {
		entry = e
		return nil
	}).Once()
	auditMW, err := middleware.NewAudit(middleware.AuditDeps{Auditor: auditor}, middleware.WithAuditType(middleware.AuditTypeS3))
	require.NoError(t, err)
	tracer := tracemock.NewService(t)
	tracer.EXPECT().Enabled().Return(true)
	var event trace.Event
	tracer.EXPECT().Publish(mock.Anything, mock.Anything).Run(func(_ context.Context, e trace.Event) { event = e }).Once()
	traceMW, err := middleware.NewTrace(middleware.TraceDeps{Tracer: tracer})
	require.NoError(t, err)

	h := middleware.Chain(s3Handler("PutObject", &domain.Identity{AccessKey: "alice", Parent: "root"}, http.StatusOK, "ok"), auditMW, traceMW)
	h.ServeHTTP(httptest.NewRecorder(), secretRequest())

	assert.Equal(t, "PutObject", entry.API.Name)
	assert.Equal(t, "s3", entry.Type)
	assert.Equal(t, http.MethodPut, entry.API.Method)
	assert.Equal(t, "/photos/a/b.jpg", entry.API.Path)
	assert.Equal(t, int64(5), entry.API.InputBytes)
	assert.Equal(t, "alice", entry.AccessKey)
	assert.Equal(t, "root", entry.ParentUser)
	assert.Equal(t, "PutObject", event.API)
	assert.Equal(t, "alice", event.User)
	raw, err := json.Marshal(entry)
	require.NoError(t, err)
	for _, s := range secrets {
		assert.NotContains(t, string(raw), s, "a secret reached the audit log")
	}
}

// TestAuditTargetOfCustomDomains: the audit records the bucket and object
// a request actually used: the origin bucket of the custom domain of an
// s3 distribution (target middleware), refused or served, and the target
// the handler records (regression: the bucket was read from the path, so
// a write to webteam/victim/payroll.txt through a custom domain was
// audited as victim/payroll.txt).
func TestAuditTargetOfCustomDomains(t *testing.T) {
	hostBuckets := httpxmock.NewHostBucketResolver(t)
	hostBuckets.EXPECT().BucketOfHost(mock.Anything).RunAndReturn(func(host string) (string, bool) {
		return "webteam", strings.HasPrefix(host, "s3take.test")
	}).Maybe()
	tests := []struct {
		name, host, path string
		handlerTarget    []string
		bucket, object   string
	}{
		{name: "custom domain, refused before the handler", host: "s3take.test:9000", path: "/victim/payroll.txt",
			bucket: "webteam", object: "victim/payroll.txt"},
		{name: "custom domain, served", host: "s3take.test", path: "/victim/payroll.txt", handlerTarget: []string{"webteam", "victim/payroll.txt"},
			bucket: "webteam", object: "victim/payroll.txt"},
		{name: "reserved path of a custom domain", host: "s3take.test", path: "/nexo/rpc/v1/peer/x"},
		{name: "virtual host", host: "photos.s3.example.com", path: "/a.jpg", bucket: "photos", object: "a.jpg"},
		// A fully qualified host is the virtual host the S3 API serves
		// (regression: a request refused before its handler was audited
		// under the first segment of its path, a bucket the client chose).
		{name: "virtual host with a trailing dot, refused before the handler", host: "victim.s3.example.com.", path: "/innocent/x",
			bucket: "victim", object: "innocent/x"},
		{name: "virtual host with a trailing dot and a port", host: "Victim.S3.Example.com.:9000", path: "/innocent/x",
			bucket: "victim", object: "innocent/x"},
		{name: "path style", host: "127.0.0.1:9000", path: "/victim/payroll.txt", bucket: "victim", object: "payroll.txt"},
		{name: "the handler decides", host: "127.0.0.1", path: "/x/y", handlerTarget: []string{"site", "index.html"}, bucket: "site", object: "index.html"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auditor := auditmock.NewService(t)
			var entry audit.Entry
			auditor.EXPECT().Log(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, e audit.Entry) error {
				entry = e
				return nil
			}).Once()
			auditMW, err := middleware.NewAudit(middleware.AuditDeps{Auditor: auditor}, middleware.WithAuditType(middleware.AuditTypeS3),
				middleware.WithAuditDomains("s3.example.com"))
			require.NoError(t, err)
			target := middleware.NewTarget(middleware.TargetDeps{HostBuckets: hostBuckets}, middleware.WithTargetDomains("s3.example.com"))
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.handlerTarget != nil {
					httpx.SetOperationTarget(r.Context(), tt.handlerTarget[0], tt.handlerTarget[1])
				}
				w.WriteHeader(http.StatusForbidden)
			})
			req := httptest.NewRequest(http.MethodPut, tt.path, nil)
			req.Host = tt.host
			middleware.Chain(handler, target, auditMW).ServeHTTP(httptest.NewRecorder(), req)
			assert.Equal(t, tt.bucket, entry.API.Bucket)
			assert.Equal(t, tt.object, entry.API.Object)
		})
	}
}
