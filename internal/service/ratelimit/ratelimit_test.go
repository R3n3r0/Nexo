package ratelimit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/ratelimit"
	"github.com/r3n3r0/nexo/internal/service/settings"
	settingsmock "github.com/r3n3r0/nexo/internal/service/settings/mock"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// newLimiter returns a limiter reading the limits *lim points to (the
// test changes them between requests: hot reload) on a fake clock.
func newLimiter(t *testing.T, lim *settings.Limits, opts ...ratelimit.Option) (ratelimit.Service, *clock.Fake) {
	t.Helper()
	var mu sync.Mutex
	s := settingsmock.NewService(t)
	s.EXPECT().Limits(mock.Anything).RunAndReturn(func(context.Context) (settings.Limits, error) {
		mu.Lock()
		defer mu.Unlock()
		return *lim, nil
	}).Maybe()
	clk := clock.NewFake(t0)
	l, err := ratelimit.New(ratelimit.Deps{Settings: s, Clock: clk}, opts...)
	require.NoError(t, err)
	return l, clk
}

func admit(t *testing.T, l ratelimit.Service, in ratelimit.Request) ratelimit.Decision {
	t.Helper()
	d, err := l.Admit(context.Background(), in)
	require.NoError(t, err)
	return d
}

func TestNewRequiresSettings(t *testing.T) {
	_, err := ratelimit.New(ratelimit.Deps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// covers: S-OPS-10 (token bucket per client address: burst, refill on the
// clock, Retry-After until the next token, no limit by default)
func TestAdmitTokenBucket(t *testing.T) {
	lim := settings.Limits{}
	l, clk := newLimiter(t, &lim)
	req := ratelimit.Request{API: ratelimit.APIS3, ClientIP: "192.0.2.1"}
	for range 1000 {
		require.True(t, admit(t, l, req).Allowed, "no limit by default")
	}

	lim.S3IP = settings.Rate{PerSecond: 2, Burst: 4}
	for i := range 4 {
		require.True(t, admit(t, l, req).Allowed, "burst request %d", i)
	}
	d := admit(t, l, req)
	assert.False(t, d.Allowed)
	assert.Equal(t, ratelimit.KindIP, d.Kind)
	assert.Equal(t, time.Second, d.RetryAfter, "half a second until the next token, rounded up to the minimum")
	assert.True(t, admit(t, l, ratelimit.Request{API: ratelimit.APIS3, ClientIP: "192.0.2.2"}).Allowed, "another client has its own bucket")
	assert.True(t, admit(t, l, ratelimit.Request{API: ratelimit.APIAdmin, ClientIP: "192.0.2.1"}).Allowed, "the Admin API has its own limits")

	clk.Advance(500 * time.Millisecond)
	assert.True(t, admit(t, l, req).Allowed, "one token earned in half a second")
	assert.False(t, admit(t, l, req).Allowed)
	clk.Advance(time.Hour)
	for range 4 {
		require.True(t, admit(t, l, req).Allowed, "refilled up to the burst, not beyond")
	}
	assert.False(t, admit(t, l, req).Allowed)

	lim.S3IP = settings.Rate{PerSecond: 1, Burst: 1}
	clk.Advance(time.Hour)
	assert.True(t, admit(t, l, req).Allowed)
	d = admit(t, l, req)
	assert.False(t, d.Allowed, "a lower limit applies at once")
	lim.S3IP = settings.Rate{}
	assert.True(t, admit(t, l, req).Allowed, "turning the limit off admits at once")
}

func TestAdmitRetryAfter(t *testing.T) {
	lim := settings.Limits{S3Bucket: settings.Rate{PerSecond: 1, Burst: 1}}
	l, clk := newLimiter(t, &lim)
	req := ratelimit.Request{API: ratelimit.APIS3, Bucket: "photos"}
	require.True(t, admit(t, l, req).Allowed)
	clk.Advance(100 * time.Millisecond)
	d := admit(t, l, req)
	require.False(t, d.Allowed)
	assert.Equal(t, ratelimit.KindBucket, d.Kind)
	assert.Equal(t, time.Second, d.RetryAfter, "at least one second")

	lim.S3AccessKey = settings.Rate{PerSecond: 1, Burst: 1}
	slow := ratelimit.Request{API: ratelimit.APIS3, User: "AKIA"}
	require.True(t, admit(t, l, slow).Allowed)
	d = admit(t, l, slow)
	require.False(t, d.Allowed)
	assert.Equal(t, ratelimit.KindUser, d.Kind)
}

// covers: S-OPS-10 (the scopes of a request: the first exhausted one
// refuses it and the tokens taken from the others are given back)
func TestAdmitScopesAllOrNothing(t *testing.T) {
	lim := settings.Limits{
		S3IP:        settings.Rate{PerSecond: 10, Burst: 2},
		S3AccessKey: settings.Rate{PerSecond: 10, Burst: 10},
		S3Bucket:    settings.Rate{PerSecond: 10, Burst: 1},
	}
	l, _ := newLimiter(t, &lim)
	require.True(t, admit(t, l, ratelimit.Request{API: ratelimit.APIS3, ClientIP: "192.0.2.1", User: "AK1", Bucket: "b"}).Allowed)
	d := admit(t, l, ratelimit.Request{API: ratelimit.APIS3, ClientIP: "192.0.2.1", User: "AK1", Bucket: "b"})
	require.False(t, d.Allowed)
	assert.Equal(t, ratelimit.KindBucket, d.Kind)
	// The IP token taken before the bucket refused was given back: the
	// burst of 2 still has one.
	assert.True(t, admit(t, l, ratelimit.Request{API: ratelimit.APIS3, ClientIP: "192.0.2.1"}).Allowed)
	assert.False(t, admit(t, l, ratelimit.Request{API: ratelimit.APIS3, ClientIP: "192.0.2.1"}).Allowed)
	assert.True(t, admit(t, l, ratelimit.Request{API: ratelimit.APIS3, User: "AK1", Bucket: "other"}).Allowed,
		"empty scopes are not limited, other buckets have their own")
}

func TestAdmitIPv6Network(t *testing.T) {
	lim := settings.Limits{AdminIP: settings.Rate{PerSecond: 1, Burst: 1}}
	l, _ := newLimiter(t, &lim)
	require.True(t, admit(t, l, ratelimit.Request{API: ratelimit.APIAdmin, ClientIP: "2001:db8:1:2::1"}).Allowed)
	assert.False(t, admit(t, l, ratelimit.Request{API: ratelimit.APIAdmin, ClientIP: "2001:db8:1:2:ffff::9"}).Allowed,
		"the addresses of a /64 share a bucket")
	assert.True(t, admit(t, l, ratelimit.Request{API: ratelimit.APIAdmin, ClientIP: "2001:db8:1:3::1"}).Allowed)
	require.True(t, admit(t, l, ratelimit.Request{API: ratelimit.APIAdmin, ClientIP: "::ffff:192.0.2.7"}).Allowed)
	assert.False(t, admit(t, l, ratelimit.Request{API: ratelimit.APIAdmin, ClientIP: "192.0.2.7"}).Allowed, "IPv4-mapped is IPv4")
}

func TestAdmitUnknownAPI(t *testing.T) {
	l, _ := newLimiter(t, &settings.Limits{})
	_, err := l.Admit(context.Background(), ratelimit.Request{API: "ftp"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestAdmitSettingsFailureAdmits(t *testing.T) {
	s := settingsmock.NewService(t)
	s.EXPECT().Limits(mock.Anything).Return(settings.Limits{}, errors.New("store down"))
	l, err := ratelimit.New(ratelimit.Deps{Settings: s})
	require.NoError(t, err)
	d, err := l.Admit(context.Background(), ratelimit.Request{API: ratelimit.APIS3, ClientIP: "192.0.2.1"})
	require.NoError(t, err)
	assert.True(t, d.Allowed, "a store failure does not take the API down")
	release, d, err := l.Acquire(context.Background())
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	release()
}

// covers: S-DST-08 (per-distribution rate limit, per client address or
// shared by the distribution; TDS-13)
func TestAdmitDistribution(t *testing.T) {
	l, clk := newLimiter(t, &settings.Limits{})
	ctx := context.Background()
	perIP := ratelimit.DistributionLimit{RequestsPerSecond: 1, Burst: 2, PerIP: true}
	in := func(id, ip string, limit ratelimit.DistributionLimit) ratelimit.DistributionRequest {
		return ratelimit.DistributionRequest{API: ratelimit.APIWeb, Distribution: id, ClientIP: ip, Limit: limit}
	}
	for range 2 {
		d, err := l.AdmitDistribution(ctx, in("site", "192.0.2.1", perIP))
		require.NoError(t, err)
		require.True(t, d.Allowed)
	}
	d, err := l.AdmitDistribution(ctx, in("site", "192.0.2.1", perIP))
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, ratelimit.KindDistribution, d.Kind)
	d, _ = l.AdmitDistribution(ctx, in("site", "192.0.2.2", perIP))
	assert.True(t, d.Allowed, "another client of the distribution")
	d, _ = l.AdmitDistribution(ctx, in("other", "192.0.2.1", perIP))
	assert.True(t, d.Allowed, "another distribution")

	shared := ratelimit.DistributionLimit{RequestsPerSecond: 1}
	d, _ = l.AdmitDistribution(ctx, in("api", "192.0.2.1", shared))
	require.True(t, d.Allowed, "burst defaults to the rate")
	d, _ = l.AdmitDistribution(ctx, in("api", "192.0.2.9", shared))
	assert.False(t, d.Allowed, "the clients share the bucket of the distribution")
	clk.Advance(time.Second)
	d, _ = l.AdmitDistribution(ctx, ratelimit.DistributionRequest{API: ratelimit.APIS3, Distribution: "api", Limit: shared})
	assert.True(t, d.Allowed, "refilled; same bucket on every listener")

	d, err = l.AdmitDistribution(ctx, in("free", "192.0.2.1", ratelimit.DistributionLimit{}))
	require.NoError(t, err)
	assert.True(t, d.Allowed, "no rate: no limit")
	_, err = l.AdmitDistribution(ctx, in("", "192.0.2.1", shared))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// The memory is bounded: beyond MaxKeys the new keys share the overflow
// bucket, and the sweep drops the buckets that are full again.
func TestMaxKeysAndSweep(t *testing.T) {
	lim := settings.Limits{S3IP: settings.Rate{PerSecond: 1, Burst: 1}}
	l, clk := newLimiter(t, &lim, ratelimit.WithMaxKeys(2))
	ip := func(s string) ratelimit.Request { return ratelimit.Request{API: ratelimit.APIS3, ClientIP: s} }
	require.True(t, admit(t, l, ip("192.0.2.1")).Allowed)
	require.True(t, admit(t, l, ip("192.0.2.2")).Allowed)
	require.True(t, admit(t, l, ip("192.0.2.3")).Allowed, "the first overflow client")
	assert.False(t, admit(t, l, ip("192.0.2.4")).Allowed, "the new clients share the overflow bucket")
	assert.False(t, admit(t, l, ip("192.0.2.1")).Allowed, "the known clients keep their bucket")

	clk.Advance(2 * time.Second)
	require.True(t, admit(t, l, ip("192.0.2.5")).Allowed, "the full buckets are dropped: a new client gets its own")
	assert.True(t, admit(t, l, ip("192.0.2.6")).Allowed)
	assert.False(t, admit(t, l, ip("192.0.2.5")).Allowed)
}

// covers: S-OPS-10 (api.requests_max: the requests beyond it wait up to
// api.requests_deadline for a slot, in arrival order, then are refused)
func TestAcquireConcurrency(t *testing.T) {
	lim := settings.Limits{}
	l, _ := newLimiter(t, &lim)
	ctx := context.Background()
	release, d, err := l.Acquire(ctx)
	require.NoError(t, err)
	require.True(t, d.Allowed, "no limit by default")
	release()

	lim.RequestsMax, lim.RequestsDeadline = 2, 0
	r1, d, err := l.Acquire(ctx)
	require.NoError(t, err)
	require.True(t, d.Allowed)
	r2, d, _ := l.Acquire(ctx)
	require.True(t, d.Allowed)
	r3, d, err := l.Acquire(ctx)
	require.NoError(t, err)
	assert.False(t, d.Allowed, "no deadline: refused at once")
	assert.Equal(t, ratelimit.KindConcurrency, d.Kind)
	assert.Equal(t, time.Second, d.RetryAfter)
	r3()

	lim.RequestsDeadline = 20 * time.Millisecond
	start := time.Now()
	_, d, err = l.Acquire(ctx)
	require.NoError(t, err)
	assert.False(t, d.Allowed, "refused after the deadline")
	assert.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)

	lim.RequestsDeadline = 10 * time.Second
	granted := make(chan ratelimit.Release)
	go func() {
		r, d, err := l.Acquire(ctx)
		if err == nil && d.Allowed {
			granted <- r
		}
		close(granted)
	}()
	time.Sleep(10 * time.Millisecond)
	r1()
	r1() // released once only
	r4, ok := <-granted
	require.True(t, ok, "the waiter gets the freed slot")
	_, d, _ = func() (ratelimit.Release, ratelimit.Decision, error) {
		lim.RequestsDeadline = 0
		return l.Acquire(ctx)
	}()
	assert.False(t, d.Allowed, "the double release did not free a second slot")

	cctx, cancel := context.WithCancel(ctx)
	lim.RequestsDeadline = 10 * time.Second
	done := make(chan error)
	go func() {
		_, _, err := l.Acquire(cctx)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled, "a canceled request stops waiting")

	lim.RequestsMax = 3
	r5, d, _ := l.Acquire(ctx)
	assert.True(t, d.Allowed, "a higher limit applies at once")
	r2()
	r4()
	r5()
}
