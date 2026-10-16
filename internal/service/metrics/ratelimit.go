package metrics

import (
	"context"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/ratelimit"
)

// WrapRateLimit returns l measured: the requests its limits refuse, by
// API (s3, admin, web) and kind (ip, user, bucket, distribution,
// concurrency), in nexo_ratelimit_throttled_total (S-OPS-10).
func WrapRateLimit(ctx context.Context, l ratelimit.Service, reg Registry) (ratelimit.Service, error) {
	if l == nil || reg == nil {
		return nil, fmt.Errorf("%w: metrics: the rate limit decorator needs a limiter and a registry", domain.ErrInvalidArgument)
	}
	throttled, err := reg.Counter(ctx, Definition{Subsystem: "ratelimit", Name: "throttled_total",
		Help:   "Requests refused by the rate limits of the node, by API and kind of limit.",
		Labels: []string{"api", "kind"}})
	if err != nil {
		return nil, fmt.Errorf("metrics: ratelimit: %w", err)
	}
	return &measuredRateLimit{next: l, throttled: throttled}, nil
}

type measuredRateLimit struct {
	next      ratelimit.Service
	throttled Counter
}

var _ ratelimit.Service = (*measuredRateLimit)(nil)

// count records a refusal.
func (m *measuredRateLimit) count(ctx context.Context, api ratelimit.API, d ratelimit.Decision, err error) {
	if err == nil && !d.Allowed {
		m.throttled.Add(ctx, 1, string(api), string(d.Kind))
	}
}

// Admit implements ratelimit.Service.
func (m *measuredRateLimit) Admit(ctx context.Context, in ratelimit.Request) (ratelimit.Decision, error) {
	d, err := m.next.Admit(ctx, in)
	m.count(ctx, in.API, d, err)
	return d, err
}

// AdmitDistribution implements ratelimit.Service.
func (m *measuredRateLimit) AdmitDistribution(ctx context.Context, in ratelimit.DistributionRequest) (ratelimit.Decision, error) {
	d, err := m.next.AdmitDistribution(ctx, in)
	m.count(ctx, in.API, d, err)
	return d, err
}

// Acquire implements ratelimit.Service: the concurrency limit is the S3
// API's.
func (m *measuredRateLimit) Acquire(ctx context.Context) (ratelimit.Release, ratelimit.Decision, error) {
	release, d, err := m.next.Acquire(ctx)
	m.count(ctx, ratelimit.APIS3, d, err)
	return release, d, err
}
