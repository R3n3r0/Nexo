package middleware

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/ratelimit"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// RateLimitDeps are the dependencies of the rate limit middlewares.
type RateLimitDeps struct {
	// Limiter decides (required).
	Limiter ratelimit.Service
	// OnError writes the refusals (required): S3ErrorWriter answers 503
	// SlowDown, ProblemErrorWriter 429 too_many_requests (both from
	// domain.ErrTooManyRequests), with Retry-After.
	OnError ErrorWriter
}

// RateLimitOption configures a rate limit middleware.
type RateLimitOption func(*rateLimitOptions)

type rateLimitOptions struct {
	api     ratelimit.API
	scopes  map[ratelimit.Kind]bool
	skip    func(r *http.Request) bool
	domains []string
}

// WithRateLimitAPI selects the limits of an API (default ratelimit.APIS3).
func WithRateLimitAPI(api ratelimit.API) RateLimitOption {
	return func(o *rateLimitOptions) {
		if api != "" {
			o.api = api
		}
	}
}

// WithRateLimitScopes selects the scopes the middleware limits (default
// the client address): ratelimit.KindIP before the authentication, so
// that a flood costs no signature verification; KindUser (the access key
// or user of the authenticated caller) and KindBucket (the bucket of the
// request, as the target middleware resolved it) after it.
func WithRateLimitScopes(kinds ...ratelimit.Kind) RateLimitOption {
	return func(o *rateLimitOptions) {
		o.scopes = make(map[ratelimit.Kind]bool, len(kinds))
		for _, k := range kinds {
			o.scopes[k] = true
		}
	}
}

// WithRateLimitSkip leaves alone the requests skip selects (the health
// probes, the metrics scrapes, the website distributions).
func WithRateLimitSkip(skip func(r *http.Request) bool) RateLimitOption {
	return func(o *rateLimitOptions) { o.skip = skip }
}

// WithRateLimitDomains names the bucket of the virtual-host requests
// ("<bucket>.<domain>") when no handler or target middleware recorded it.
func WithRateLimitDomains(domains ...string) RateLimitOption {
	return func(o *rateLimitOptions) { o.domains = domains }
}

type rateLimit struct {
	deps RateLimitDeps
	opts rateLimitOptions
}

var _ Middleware = (*rateLimit)(nil)

// NewRateLimit returns the middleware that admits the requests through
// the token buckets of the limiter (S-OPS-10). A refused request gets
// the error of OnError with Retry-After, and is marked for the audit log
// and the live trace (httpx.SetOperationThrottled): place it inside them.
// A missing limiter or error writer is a wiring error
// (domain.ErrInvalidArgument).
func NewRateLimit(deps RateLimitDeps, opts ...RateLimitOption) (Middleware, error) {
	if deps.Limiter == nil || deps.OnError == nil {
		return nil, fmt.Errorf("%w: the rate limit middleware requires the limiter and the error writer", domain.ErrInvalidArgument)
	}
	o := rateLimitOptions{api: ratelimit.APIS3, scopes: map[ratelimit.Kind]bool{ratelimit.KindIP: true}}
	for _, opt := range opts {
		opt(&o)
	}
	return &rateLimit{deps: deps, opts: o}, nil
}

// Wrap implements Middleware.
func (m *rateLimit) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.opts.skip != nil && m.opts.skip(r) {
			next.ServeHTTP(w, r)
			return
		}
		in := ratelimit.Request{API: m.opts.api}
		if m.opts.scopes[ratelimit.KindIP] {
			in.ClientIP = ClientOf(r).IP
		}
		if m.opts.scopes[ratelimit.KindUser] {
			if id, ok := IdentityFromContext(r.Context()); authenticated(id, ok) {
				in.User = id.AccessKey
			}
		}
		if m.opts.scopes[ratelimit.KindBucket] {
			in.Bucket, _ = targetOf(r, m.opts.domains)
		}
		d, err := m.deps.Limiter.Admit(r.Context(), in)
		if err != nil {
			m.deps.OnError(w, r, err)
			return
		}
		if !d.Allowed {
			Throttle(w, r, d, m.deps.OnError)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Throttle answers a request a limit refused: Retry-After, the mark of the
// observers and the error of onError (domain.ErrTooManyRequests).
func Throttle(w http.ResponseWriter, r *http.Request, d ratelimit.Decision, onError ErrorWriter) {
	w.Header().Set("Retry-After", RetryAfter(d.RetryAfter))
	httpx.SetOperationThrottled(r.Context(), string(d.Kind))
	onError(w, r, fmt.Errorf("%w: the %s rate limit is exceeded, retry later", domain.ErrTooManyRequests, d.Kind))
}

// RetryAfter is the value of the Retry-After header: whole seconds,
// rounded up, at least 1.
func RetryAfter(d time.Duration) string {
	return strconv.Itoa(max(1, int(math.Ceil(d.Seconds()))))
}

// ConcurrencyLimitOption configures the concurrency limit middleware.
type ConcurrencyLimitOption func(*concurrencyLimit)

// WithConcurrencySkip leaves alone the requests skip selects.
func WithConcurrencySkip(skip func(r *http.Request) bool) ConcurrencyLimitOption {
	return func(c *concurrencyLimit) { c.skip = skip }
}

type concurrencyLimit struct {
	deps RateLimitDeps
	skip func(r *http.Request) bool
}

var _ Middleware = (*concurrencyLimit)(nil)

// NewConcurrencyLimit returns the middleware that serves at most
// api.requests_max requests at once (Limiter.Acquire): a request beyond
// it waits up to api.requests_deadline for a slot, then gets the error of
// OnError (503 SlowDown on the S3 API) with Retry-After. A missing
// limiter or error writer is a wiring error (domain.ErrInvalidArgument).
func NewConcurrencyLimit(deps RateLimitDeps, opts ...ConcurrencyLimitOption) (Middleware, error) {
	if deps.Limiter == nil || deps.OnError == nil {
		return nil, fmt.Errorf("%w: the concurrency limit middleware requires the limiter and the error writer", domain.ErrInvalidArgument)
	}
	c := &concurrencyLimit{deps: deps}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Wrap implements Middleware.
func (m *concurrencyLimit) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.skip != nil && m.skip(r) {
			next.ServeHTTP(w, r)
			return
		}
		release, d, err := m.deps.Limiter.Acquire(r.Context())
		defer release()
		switch {
		case err != nil:
			// The client went away while waiting.
			m.deps.OnError(w, r, err)
		case !d.Allowed:
			Throttle(w, r, d, m.deps.OnError)
		default:
			next.ServeHTTP(w, r)
		}
	})
}
