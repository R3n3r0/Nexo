package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/metrics"
)

// APIMetricsDeps are the dependencies of the S3 API metrics middleware.
type APIMetricsDeps struct {
	// Registry creates the instruments (required).
	Registry metrics.Registry
	// Clock measures latencies (default wall clock).
	Clock clock.Clock
}

// APIMetricsOption configures the S3 API metrics middleware.
type APIMetricsOption func(*apiMetricsOptions)

type apiMetricsOptions struct {
	domains     []string
	bucketLimit int
}

// WithAPIMetricsDomains names the bucket of the virtual-host requests.
func WithAPIMetricsDomains(domains ...string) APIMetricsOption {
	return func(o *apiMetricsOptions) { o.domains = domains }
}

// WithBucketLabelLimit bounds the distinct buckets of the bucket families
// (default 1000): the requests of the others are counted under "_other".
func WithBucketLabelLimit(n int) APIMetricsOption {
	return func(o *apiMetricsOptions) {
		if n > 0 {
			o.bucketLimit = n
		}
	}
}

// otherBuckets labels the buckets beyond the limit.
const otherBuckets = "_other"

// unknownAPI labels the requests no route named (refused by the
// authentication, malformed).
const unknownAPI = "unknown"

type apiMetrics struct {
	opts     apiMetricsOptions
	clock    clock.Clock
	requests metrics.Counter
	duration metrics.Histogram
	err4xx   metrics.Counter
	err5xx   metrics.Counter
	inflight metrics.Gauge
	rx       metrics.Counter
	tx       metrics.Counter

	bucketRequests metrics.Counter
	bucketRx       metrics.Counter
	bucketTx       metrics.Counter

	mu      sync.Mutex
	buckets map[string]bool
}

var _ Middleware = (*apiMetrics)(nil)

// NewAPIMetrics returns the middleware of the S3 API metrics (docs/07
// §7.4): requests by API and status, latency histogram by API, 4xx and 5xx
// errors by API, requests in flight, bytes received and sent, and the
// bucket families (nexo_bucket_*: requests by bucket, API and status
// class, traffic by bucket). The API is the operation the router names
// (httpx.SetOperationName). A bucket labels a series only when the request
// proves it exists or concerns a caller of it (an answer below 400, a
// server error, or an authenticated request not answered 404), and at most
// WithBucketLabelLimit distinct buckets are labeled: an anonymous client
// cannot grow the series with random names. Object keys never label a
// series. A missing registry is a wiring error (domain.ErrInvalidArgument).
func NewAPIMetrics(deps APIMetricsDeps, opts ...APIMetricsOption) (Middleware, error) {
	if deps.Registry == nil {
		return nil, fmt.Errorf("%w: API metrics middleware requires a registry", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	o := apiMetricsOptions{bucketLimit: 1000}
	for _, opt := range opts {
		opt(&o)
	}
	m := &apiMetrics{opts: o, clock: deps.Clock, buckets: map[string]bool{}}
	ctx := context.Background()
	reg := deps.Registry
	var err error
	counter := func(dst *metrics.Counter, subsystem, name, help string, labels ...string) {
		if err == nil {
			*dst, err = reg.Counter(ctx, metrics.Definition{Subsystem: subsystem, Name: name, Help: help, Labels: labels})
		}
	}
	counter(&m.requests, "s3", "requests_total", "S3 requests by API and status.", "api", "status")
	counter(&m.err4xx, "s3", "requests_4xx_errors_total", "S3 requests answered with a 4xx status, by API.", "api")
	counter(&m.err5xx, "s3", "requests_5xx_errors_total", "S3 requests answered with a 5xx status, by API.", "api")
	counter(&m.rx, "s3", "traffic_received_bytes_total", "Bytes of the bodies of the S3 requests.")
	counter(&m.tx, "s3", "traffic_sent_bytes_total", "Bytes of the bodies of the S3 answers.")
	counter(&m.bucketRequests, "bucket", "requests_total", "S3 requests by bucket, API and status class.", "bucket", "api", "status_class")
	counter(&m.bucketRx, "bucket", "traffic_received_bytes_total", "Bytes of the bodies of the S3 requests, by bucket.", "bucket")
	counter(&m.bucketTx, "bucket", "traffic_sent_bytes_total", "Bytes of the bodies of the S3 answers, by bucket.", "bucket")
	if err == nil {
		m.duration, err = reg.Histogram(ctx, metrics.Definition{Subsystem: "s3", Name: "request_duration_seconds",
			Help: "Latency of the S3 requests by API.", Labels: []string{"api"}})
	}
	if err == nil {
		m.inflight, err = reg.Gauge(ctx, metrics.Definition{Subsystem: "s3", Name: "requests_inflight",
			Help: "S3 requests being served."})
	}
	if err != nil {
		return nil, fmt.Errorf("API metrics middleware: %w", err)
	}
	return m, nil
}

// Wrap implements Middleware.
func (m *apiMetrics) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, holder := observe(r.Context())
		r = r.WithContext(ctx)
		m.inflight.Add(ctx, 1)
		defer m.inflight.Add(ctx, -1)
		start := m.clock.Now()
		received := countBody(r)
		rw := wrapWriter(w)
		next.ServeHTTP(rw, r)

		api := holder.Name()
		if api == "" {
			api = unknownAPI
		}
		status := rw.Status()
		m.requests.Add(ctx, 1, api, strconv.Itoa(status))
		m.duration.Observe(ctx, m.clock.Now().Sub(start).Seconds(), api)
		switch {
		case status >= 500:
			m.err5xx.Add(ctx, 1, api)
		case status >= 400:
			m.err4xx.Add(ctx, 1, api)
		}
		in, out := received(), rw.bytes
		m.rx.Add(ctx, float64(in))
		m.tx.Add(ctx, float64(out))
		bucket, _ := targetOf(r, m.opts.domains)
		if bucket == "" {
			return
		}
		id, ok := callerOf(ctx)
		if status >= 400 && status < 500 && (status == http.StatusNotFound || !authenticated(id, ok)) {
			return
		}
		bucket = m.label(bucket)
		m.bucketRequests.Add(ctx, 1, bucket, api, strconv.Itoa(status/100)+"xx")
		m.bucketRx.Add(ctx, float64(in), bucket)
		m.bucketTx.Add(ctx, float64(out), bucket)
	})
}

// label returns the label of a bucket, otherBuckets beyond the limit.
func (m *apiMetrics) label(bucket string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.buckets[bucket] {
		return bucket
	}
	if len(m.buckets) >= m.opts.bucketLimit {
		return otherBuckets
	}
	m.buckets[bucket] = true
	return bucket
}
