package middleware

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

type fixedIDs struct{}

func (fixedIDs) NewRequestID() string { return "REQ" }
func (fixedIDs) NewHostID() string    { return "HOST" }

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	})
}

func TestChainOrder(t *testing.T) {
	var order []string
	mk := func(name string) Middleware {
		return Func(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		})
	}
	h := Chain(okHandler(), mk("a"), nil, mk("b"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, []string{"a", "b"}, order)
	assert.Equal(t, http.StatusTeapot, rec.Code)
}

func TestRequestID(t *testing.T) {
	var got string
	h := NewRequestID(fixedIDs{}).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = httpx.RequestIDFrom(r.Context())
		assert.Equal(t, "HOST", httpx.HostIDFrom(r.Context()))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, "REQ", got)
	assert.Equal(t, "REQ", rec.Header().Get("x-amz-request-id"))
	assert.Equal(t, "HOST", rec.Header().Get("x-amz-id-2"))

	// Default generator produces distinct ids.
	h = NewRequestID(nil).Wrap(okHandler())
	r1, r2 := httptest.NewRecorder(), httptest.NewRecorder()
	h.ServeHTTP(r1, httptest.NewRequest(http.MethodGet, "/", nil))
	h.ServeHTTP(r2, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.NotEqual(t, r1.Header().Get("x-amz-request-id"), r2.Header().Get("x-amz-request-id"))
}

func TestRecover(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })

	h := NewRecover(logger, nil).Wrap(panicking)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/b/k", nil))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "<Code>InternalError</Code>")
	assert.Contains(t, buf.String(), "boom")
	assert.Contains(t, buf.String(), "stack")

	// Problem flavor.
	h = NewRecover(logger, ProblemErrorWriter(httpx.New())).Wrap(panicking)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))
	assert.Equal(t, 500, rec.Code)
	assert.Equal(t, httpx.ContentTypeProblem, rec.Header().Get("Content-Type"))

	// ErrAbortHandler propagates.
	abort := NewRecover(logger, nil).Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	assert.Panics(t, func() { abort.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil)) })
}

func TestLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := Chain(okHandler(), NewRequestID(fixedIDs{}), NewLogging(logger, WithLogLevel(slog.LevelInfo)))
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
}

func TestCORS(t *testing.T) {
	h := NewCORS(CORSOptions{}).Wrap(okHandler())
	req := httptest.NewRequest(http.MethodOptions, "/b", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "PUT")

	restricted := NewCORS(CORSOptions{AllowedOrigins: []string{"https://ok.example.com"}}).Wrap(okHandler())
	req = httptest.NewRequest(http.MethodGet, "/b", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec = httptest.NewRecorder()
	restricted.ServeHTTP(rec, req)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	req.Header.Set("Origin", "https://ok.example.com")
	rec = httptest.NewRecorder()
	restricted.ServeHTTP(rec, req)
	assert.Equal(t, "https://ok.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, strings.ToLower(rec.Header().Get("Access-Control-Expose-Headers")), "etag")
}

func TestMetrics(t *testing.T) {
	reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
	m, err := NewMetrics(reg, "s3")
	require.NoError(t, err)
	// Registering twice on the same registry is tolerated.
	_, err = NewMetrics(reg, "s3")
	require.NoError(t, err)

	h := m.Wrap(okHandler())
	for i := 0; i < 3; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PROPFIND", "/", nil))

	families, err := reg.Gather()
	require.NoError(t, err)
	found := map[string]bool{}
	for _, f := range families {
		found[f.GetName()] = true
		if f.GetName() == "nexo_http_requests_total" {
			var total float64
			for _, mt := range f.GetMetric() {
				total += mt.GetCounter().GetValue()
				labels := map[string]string{}
				for _, l := range mt.GetLabel() {
					labels[l.GetName()] = l.GetValue()
				}
				assert.Equal(t, "418", labels["status"])
				assert.Contains(t, []string{"GET", "OTHER"}, labels["method"])
			}
			assert.Equal(t, float64(4), total)
		}
	}
	assert.True(t, found["nexo_http_requests_total"])
	assert.True(t, found["nexo_http_request_duration_seconds"])
	assert.True(t, found["nexo_http_requests_in_flight"])
}

type fakeVerifier struct {
	id  *domain.Identity
	err error
}

func (f fakeVerifier) Verify(context.Context, *http.Request) (*domain.Identity, error) {
	return f.id, f.err
}
func (f fakeVerifier) VerifyPresigned(context.Context, *http.Request) (*domain.Identity, error) {
	return f.id, f.err
}

func TestAuth(t *testing.T) {
	var got *domain.Identity
	var ok bool
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = IdentityFromContext(r.Context())
	})

	// No verifier: anonymous.
	NewAuth(nil, nil).Wrap(capture).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	require.True(t, ok)
	assert.True(t, got.IsAnonymous)

	// Verified identity.
	v := verifierAdapter{fakeVerifier{id: &domain.Identity{AccessKey: "alice"}}}
	NewAuth(v, nil).Wrap(capture).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, "alice", got.AccessKey)

	// Failure writes the S3 error.
	v = verifierAdapter{fakeVerifier{err: fmt.Errorf("sig: %w", domain.ErrSignatureMismatch)}}
	rec := httptest.NewRecorder()
	NewAuth(v, nil).Wrap(capture).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/b", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "SignatureDoesNotMatch")

	// Without middleware the context has no identity.
	_, ok = IdentityFromContext(context.Background())
	assert.False(t, ok)
}

func TestMaxBody(t *testing.T) {
	var readErr error
	h := NewMaxBody(4).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	// Disabled limit returns the handler unchanged.
	passthrough := okHandler()
	assert.NotNil(t, NewMaxBody(0).Wrap(passthrough))
}
