package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/audit"
	auditmock "github.com/r3n3r0/nexo/internal/service/audit/mock"
	"github.com/r3n3r0/nexo/internal/service/ratelimit"
	ratelimitmock "github.com/r3n3r0/nexo/internal/service/ratelimit/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

func TestNewRateLimitRequiresDeps(t *testing.T) {
	_, err := middleware.NewRateLimit(middleware.RateLimitDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = middleware.NewRateLimit(middleware.RateLimitDeps{Limiter: ratelimitmock.NewService(t)})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = middleware.NewConcurrencyLimit(middleware.RateLimitDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// covers: S-OPS-10 (the S3 API answers 503 SlowDown in S3 XML with
// Retry-After; the request is marked in the audit log)
func TestRateLimitS3SlowDown(t *testing.T) {
	codec := httpx.New()
	limiter := ratelimitmock.NewService(t)
	limiter.EXPECT().Admit(mock.Anything, ratelimit.Request{API: ratelimit.APIS3, ClientIP: "203.0.113.9"}).
		Return(ratelimit.Decision{Kind: ratelimit.KindIP, RetryAfter: 2500 * time.Millisecond}, nil).Once()
	limiter.EXPECT().Admit(mock.Anything, ratelimit.Request{API: ratelimit.APIS3, ClientIP: "203.0.113.10"}).
		Return(ratelimit.Decision{Allowed: true}, nil).Once()
	rl, err := middleware.NewRateLimit(middleware.RateLimitDeps{Limiter: limiter, OnError: middleware.S3ErrorWriter(codec)},
		middleware.WithRateLimitSkip(func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/nexo/") }))
	require.NoError(t, err)
	auditor := auditmock.NewService(t)
	var entries []audit.Entry
	auditor.EXPECT().Log(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, e audit.Entry) error {
		entries = append(entries, e)
		return nil
	})
	auditing, err := middleware.NewAudit(middleware.AuditDeps{Auditor: auditor})
	require.NoError(t, err)
	fwd, err := middleware.NewForwarded(middleware.ForwardedDeps{})
	require.NoError(t, err)
	h := middleware.Chain(okHandler(), fwd, auditing, rl)

	req := httptest.NewRequest(http.MethodGet, "/photos/a.jpg", nil)
	req.RemoteAddr = "203.0.113.9:4000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "3", rec.Header().Get("Retry-After"), "rounded up to whole seconds")
	assert.Contains(t, rec.Body.String(), "<Code>SlowDown</Code>")
	assert.Contains(t, rec.Body.String(), "Please reduce your request rate.")
	require.Len(t, entries, 1)
	assert.Equal(t, map[string]any{middleware.AuditTagThrottled: "ip"}, entries[0].Tags)
	assert.Equal(t, "Service Unavailable (rate limit: ip)", entries[0].Error)

	req = httptest.NewRequest(http.MethodGet, "/photos/a.jpg", nil)
	req.RemoteAddr = "203.0.113.10:4000"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTeapot, rec.Code)
	require.Len(t, entries, 2)
	assert.Nil(t, entries[1].Tags)

	// The health probes are exempt: the limiter is not asked.
	req = httptest.NewRequest(http.MethodGet, domain.HealthLivePath, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTeapot, rec.Code)
}

// covers: S-OPS-10 (the Admin API answers 429 problem+json; the scopes
// after the authentication: the user and the bucket of the request)
func TestRateLimitProblemAndScopes(t *testing.T) {
	codec := httpx.New()
	limiter := ratelimitmock.NewService(t)
	limiter.EXPECT().Admit(mock.Anything, ratelimit.Request{API: ratelimit.APIAdmin, User: "alice", Bucket: "photos"}).
		Return(ratelimit.Decision{Kind: ratelimit.KindUser, RetryAfter: time.Second}, nil).Once()
	limiter.EXPECT().Admit(mock.Anything, ratelimit.Request{API: ratelimit.APIAdmin, Bucket: "photos"}).
		Return(ratelimit.Decision{Allowed: true}, nil).Once()
	rl, err := middleware.NewRateLimit(middleware.RateLimitDeps{Limiter: limiter, OnError: middleware.ProblemErrorWriter(codec)},
		middleware.WithRateLimitAPI(ratelimit.APIAdmin), middleware.WithRateLimitScopes(ratelimit.KindUser, ratelimit.KindBucket))
	require.NoError(t, err)

	withID := func(id *domain.Identity) middleware.Middleware {
		return middleware.Func(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(middleware.WithIdentity(r.Context(), id)))
			})
		})
	}
	rec := httptest.NewRecorder()
	middleware.Chain(okHandler(), withID(&domain.Identity{AccessKey: "alice"}), rl).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/photos/x", nil))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	var p httpx.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	assert.Equal(t, "too_many_requests", p.Code)
	assert.Contains(t, p.Detail, "user rate limit")

	rec = httptest.NewRecorder()
	middleware.Chain(okHandler(), withID(&domain.Identity{IsAnonymous: true}), rl).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/photos/x", nil))
	assert.Equal(t, http.StatusTeapot, rec.Code, "anonymous callers have no user bucket")

	failing := ratelimitmock.NewService(t)
	failing.EXPECT().Admit(mock.Anything, mock.Anything).Return(ratelimit.Decision{}, domain.ErrInvalidArgument).Once()
	rl, err = middleware.NewRateLimit(middleware.RateLimitDeps{Limiter: failing, OnError: middleware.ProblemErrorWriter(codec)})
	require.NoError(t, err)
	rec = httptest.NewRecorder()
	middleware.Chain(okHandler(), rl).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// covers: S-OPS-10 (api.requests_max: the slot is held while the request
// is served and released after; a refusal is 503 SlowDown)
func TestConcurrencyLimit(t *testing.T) {
	codec := httpx.New()
	limiter := ratelimitmock.NewService(t)
	released := 0
	limiter.EXPECT().Acquire(mock.Anything).Return(func() { released++ }, ratelimit.Decision{Allowed: true}, nil).Once()
	limiter.EXPECT().Acquire(mock.Anything).Return(func() { released++ }, ratelimit.Decision{Kind: ratelimit.KindConcurrency, RetryAfter: time.Second}, nil).Once()
	limiter.EXPECT().Acquire(mock.Anything).Return(func() {}, ratelimit.Decision{}, context.Canceled).Once()
	cl, err := middleware.NewConcurrencyLimit(middleware.RateLimitDeps{Limiter: limiter, OnError: middleware.S3ErrorWriter(codec)},
		middleware.WithConcurrencySkip(func(r *http.Request) bool { return r.URL.Path == domain.HealthReadyPath }))
	require.NoError(t, err)
	served := false
	h := cl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		if r.URL.Path != domain.HealthReadyPath {
			assert.Equal(t, 0, released, "the slot is held while the request is served")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/b", nil))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, served)
	assert.Equal(t, 1, released)

	served = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/b", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	assert.Contains(t, rec.Body.String(), "<Code>SlowDown</Code>")
	assert.False(t, served)
	assert.Equal(t, 2, released, "the release of a refusal is called too")

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/b", nil))
	assert.False(t, served)
	assert.NotEqual(t, http.StatusNoContent, rec.Code, "a client gone while waiting is not served")

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, domain.HealthReadyPath, nil))
	assert.Equal(t, http.StatusNoContent, rec.Code, "exempt")
}

func TestRetryAfter(t *testing.T) {
	assert.Equal(t, "1", middleware.RetryAfter(0))
	assert.Equal(t, "1", middleware.RetryAfter(time.Millisecond))
	assert.Equal(t, "2", middleware.RetryAfter(1001*time.Millisecond))
	assert.Equal(t, "60", middleware.RetryAfter(time.Minute))
}

func TestThrottleMarksTrace(t *testing.T) {
	ctx, holder := httpx.WithOperationHolder(context.Background())
	rec := httptest.NewRecorder()
	var got error
	middleware.Throttle(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx),
		ratelimit.Decision{Kind: ratelimit.KindBucket, RetryAfter: 3 * time.Second},
		func(w http.ResponseWriter, _ *http.Request, err error) {
			got = err
			w.WriteHeader(http.StatusServiceUnavailable)
		})
	assert.Equal(t, "bucket", holder.Throttled())
	assert.True(t, errors.Is(got, domain.ErrTooManyRequests))
	assert.Equal(t, "3", rec.Header().Get("Retry-After"))
}
