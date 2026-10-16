package ratelimit

import (
	"context"
	"fmt"
	"hash/maphash"
	"log/slog"
	"math"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/settings"
)

// DefaultMaxKeys is the default number of buckets a node keeps
// (WithMaxKeys): about 10 MB.
const DefaultMaxKeys = 100_000

// Option configures the rate limiter.
type Option func(*options)

type options struct {
	maxKeys int
}

// WithMaxKeys bounds the buckets the node keeps (default DefaultMaxKeys):
// beyond it, after the full buckets are dropped, the new keys of a kind
// share one overflow bucket.
func WithMaxKeys(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxKeys = n
		}
	}
}

// Sweeps of the buckets that are full again: every sweepInterval, every
// fullSweepInterval while the limiter holds MaxKeys buckets.
const (
	sweepInterval     = time.Minute
	fullSweepInterval = time.Second
	shardCount        = 64
	// overflowKey is the value of the bucket shared by the new keys of a
	// kind once the limiter is full.
	overflowKey = "\x00overflow"
	// minRetryAfter is the least Retry-After of a refusal.
	minRetryAfter = time.Second
)

// bucket is a token bucket, with the rate it was last used with.
type bucket struct {
	tokens float64
	last   time.Time
	rate   float64
	burst  float64
}

// refill adds the tokens earned since the last use, up to the burst of r.
func (b *bucket) refill(now time.Time, r settings.Rate) {
	b.rate, b.burst = float64(r.PerSecond), float64(max(r.Burst, 1))
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * b.rate
		b.last = now
	}
	b.tokens = min(b.tokens, b.burst)
}

// full reports a bucket that would be full at now: dropping it changes
// nothing.
func (b *bucket) full(now time.Time) bool {
	return b.tokens+now.Sub(b.last).Seconds()*b.rate >= b.burst
}

type shard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type limiter struct {
	deps   Deps
	opts   options
	seed   maphash.Seed
	shards [shardCount]shard
	// size is the number of buckets; lastSweep the time of the last sweep
	// (Unix nanoseconds).
	size      atomic.Int64
	lastSweep atomic.Int64
	slots     slots
}

var _ Service = (*limiter)(nil)

// New returns the rate limiter. Missing settings are a wiring error
// (domain.ErrInvalidArgument).
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Settings == nil {
		return nil, fmt.Errorf("%w: the rate limiter requires the settings", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	l := &limiter{deps: deps, opts: options{maxKeys: DefaultMaxKeys}, seed: maphash.MakeSeed()}
	for _, o := range opts {
		o(&l.opts)
	}
	for i := range l.shards {
		l.shards[i].buckets = make(map[string]*bucket)
	}
	l.lastSweep.Store(deps.Clock.Now().UnixNano())
	return l, nil
}

// limits reads the applied limits; ok is false when they cannot be read
// (logged): the request is then admitted, a store failure must not take
// the APIs down.
func (l *limiter) limits(ctx context.Context) (settings.Limits, bool) {
	lim, err := l.deps.Settings.Limits(ctx)
	if err != nil {
		l.deps.Logger.WarnContext(ctx, "ratelimit: the limits cannot be read, admitting the request", "err", err)
		return settings.Limits{}, false
	}
	return lim, true
}

// scope is a bucket a request takes a token from.
type scope struct {
	kind  Kind
	value string
	rate  settings.Rate
}

// Admit implements Service.
func (l *limiter) Admit(ctx context.Context, in Request) (Decision, error) {
	var ip, user, bucket settings.Rate
	lim, ok := l.limits(ctx)
	switch in.API {
	case APIS3:
		ip, user, bucket = lim.S3IP, lim.S3AccessKey, lim.S3Bucket
	case APIAdmin:
		ip, user = lim.AdminIP, lim.AdminUser
	case APIWeb:
	default:
		return Decision{}, fmt.Errorf("%w: ratelimit: unknown API %q", domain.ErrInvalidArgument, in.API)
	}
	if !ok {
		return Decision{Allowed: true}, nil
	}
	scopes := [...]scope{
		{kind: KindIP, value: clientKey(in.ClientIP), rate: ip},
		{kind: KindUser, value: in.User, rate: user},
		{kind: KindBucket, value: in.Bucket, rate: bucket},
	}
	return l.take(in.API, scopes[:]), nil
}

// AdmitDistribution implements Service.
func (l *limiter) AdmitDistribution(_ context.Context, in DistributionRequest) (Decision, error) {
	if in.Limit.RequestsPerSecond <= 0 {
		return Decision{Allowed: true}, nil
	}
	if in.Distribution == "" {
		return Decision{}, fmt.Errorf("%w: ratelimit: the distribution has no id", domain.ErrInvalidArgument)
	}
	rate := settings.Rate{PerSecond: in.Limit.RequestsPerSecond, Burst: in.Limit.Burst}
	if rate.Burst <= 0 {
		rate.Burst = rate.PerSecond
	}
	value := in.Distribution
	if in.Limit.PerIP {
		value += "|" + clientKey(in.ClientIP)
	}
	// The buckets of a distribution are the same whichever listener (and
	// API) serves it.
	return l.take("dist", []scope{{kind: KindDistribution, value: value, rate: rate}}), nil
}

// take takes a token from every limited scope, or none.
func (l *limiter) take(api API, scopes []scope) Decision {
	now := l.deps.Clock.Now()
	l.maybeSweep(now)
	taken := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if s.value == "" || s.rate.PerSecond <= 0 {
			continue
		}
		key, ok, retry := l.takeOne(string(api)+"|"+string(s.kind), s.value, s.rate, now)
		if !ok {
			for _, k := range taken {
				l.giveBack(k)
			}
			return Decision{Kind: s.kind, RetryAfter: max(retry, minRetryAfter)}
		}
		taken = append(taken, key)
	}
	return Decision{Allowed: true}
}

// shardOf returns the shard of a key.
func (l *limiter) shardOf(key string) *shard {
	return &l.shards[maphash.String(l.seed, key)%shardCount]
}

// takeOne takes a token from the bucket of prefix|value (the overflow
// bucket of prefix when the limiter is full); it returns the key of the
// bucket, or how long until it has a token.
func (l *limiter) takeOne(prefix, value string, r settings.Rate, now time.Time) (string, bool, time.Duration) {
	key := prefix + "|" + value
	sh := l.shardOf(key)
	sh.mu.Lock()
	b, ok := sh.buckets[key]
	if !ok && l.size.Load() >= int64(l.opts.maxKeys) {
		sh.mu.Unlock()
		key = prefix + "|" + overflowKey
		sh = l.shardOf(key)
		sh.mu.Lock()
		b, ok = sh.buckets[key]
	}
	if !ok {
		b = &bucket{tokens: float64(max(r.Burst, 1)), last: now}
		sh.buckets[key] = b
		l.size.Add(1)
	}
	defer sh.mu.Unlock()
	b.refill(now, r)
	if b.tokens >= 1 {
		b.tokens--
		return key, true, 0
	}
	wait := (1 - b.tokens) / b.rate
	return key, false, time.Duration(math.Ceil(wait * float64(time.Second)))
}

// giveBack returns a token taken from the bucket of key.
func (l *limiter) giveBack(key string) {
	sh := l.shardOf(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if b, ok := sh.buckets[key]; ok {
		b.tokens = min(b.tokens+1, b.burst)
	}
}

// maybeSweep drops the buckets that are full again, every sweepInterval
// (every fullSweepInterval while the limiter is full); one caller sweeps.
func (l *limiter) maybeSweep(now time.Time) {
	last := l.lastSweep.Load()
	interval := sweepInterval
	if l.size.Load() >= int64(l.opts.maxKeys) {
		interval = fullSweepInterval
	}
	if now.UnixNano()-last < int64(interval) || !l.lastSweep.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	for i := range l.shards {
		sh := &l.shards[i]
		sh.mu.Lock()
		for k, b := range sh.buckets {
			if b.full(now) {
				delete(sh.buckets, k)
				l.size.Add(-1)
			}
		}
		sh.mu.Unlock()
	}
}

// clientKey is the key of a client address: an IPv6 client is its /64
// network (a single host usually owns one), so that it cannot pick a new
// address for every request.
func clientKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

// Acquire implements Service.
func (l *limiter) Acquire(ctx context.Context) (Release, Decision, error) {
	lim, ok := l.limits(ctx)
	if !ok || lim.RequestsMax <= 0 {
		l.slots.resize(0)
		return func() {}, Decision{Allowed: true}, nil
	}
	return l.slots.acquire(ctx, lim.RequestsMax, lim.RequestsDeadline)
}
