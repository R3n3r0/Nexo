// Package ratelimit is the request rate limiter of a node (S-OPS-10,
// S-DST-08): token buckets keyed by client address, access key or user,
// bucket and distribution, and the concurrency limit of the S3 API
// (api.requests_max, api.requests_deadline).
//
// The limits come from the dynamic configuration (settings.Limits), read
// at every request: a change written through the Admin API, replicated to
// the peers by the configuration notifier, applies to the next request of
// every node. The buckets live in the memory of the node: every limit is
// per node, so a cluster of N nodes behind a load balancer admits up to N
// times each limit (a deliberate choice: no internode call per request,
// no shared state to lose when a node fails; docs/user/configuration.md).
//
// The memory is bounded: the client addresses of IPv6 are grouped by
// their /64 network, the buckets idle long enough to be full again are
// dropped, and beyond MaxKeys buckets the new keys of a kind share one
// overflow bucket.
package ratelimit

import (
	"context"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/settings"
)

// Service admits the requests of a node.
type Service interface {
	// Admit takes one token from the bucket of every scope in sets
	// (ClientIP, User, Bucket), in this order, with the limits of in.API.
	// The first exhausted bucket refuses the request (the tokens taken
	// from the others are given back) and names its Kind. A scope whose
	// value is empty, or whose limit is not configured, is not limited.
	Admit(ctx context.Context, in Request) (Decision, error)
	// AdmitDistribution takes one token from the bucket of a distribution
	// (S-DST-08, docs/10 TDS-13): shared by every client, or one per
	// client address (Limit.PerIP). A zero rate is no limit.
	AdmitDistribution(ctx context.Context, in DistributionRequest) (Decision, error)
	// Acquire takes a slot of the S3 requests the node serves at once
	// (api.requests_max): when none is free the request waits up to
	// api.requests_deadline, then is refused with KindConcurrency. The
	// returned Release (never nil) frees the slot; it must be called once
	// the request is served, also when refused (it does nothing then).
	// A canceled ctx ends the wait with its error.
	Acquire(ctx context.Context) (Release, Decision, error)
}

// Release frees a slot taken by Acquire.
type Release func()

// API is the API a request is addressed to: it selects the limits of
// Admit and labels the metrics.
type API string

// APIs.
const (
	APIS3    API = "s3"
	APIAdmin API = "admin"
	// APIWeb are the requests of the website distributions.
	APIWeb API = "web"
)

// Kind is the scope of a limit.
type Kind string

// Kinds of limits.
const (
	KindIP           Kind = "ip"
	KindUser         Kind = "user"
	KindBucket       Kind = "bucket"
	KindDistribution Kind = "distribution"
	KindConcurrency  Kind = "concurrency"
)

// Request is a request to admit.
type Request struct {
	API API
	// ClientIP is the address of the client behind the trusted proxies.
	ClientIP string
	// User is the access key (S3) or user (Admin API) of an
	// authenticated caller; empty for anonymous requests.
	User string
	// Bucket is the bucket the request addresses (S3).
	Bucket string
}

// DistributionLimit is the rate limit of a distribution
// (restrictions.rateLimit).
type DistributionLimit struct {
	// RequestsPerSecond is the rate (0: no limit); Burst the size of the
	// bucket (0: RequestsPerSecond).
	RequestsPerSecond int
	Burst             int
	// PerIP gives every client address its own bucket ("per: ip", the
	// default); otherwise the clients share the bucket of the
	// distribution ("per: distribution").
	PerIP bool
}

// DistributionRequest is a request of a distribution to admit.
type DistributionRequest struct {
	// API is APIWeb for a website distribution, APIS3 for an s3 one.
	API          API
	Distribution string
	Limit        DistributionLimit
	ClientIP     string
}

// Decision is the outcome of an admission.
type Decision struct {
	Allowed bool
	// Kind is the limit that refused the request.
	Kind Kind
	// RetryAfter is when the client may retry (at least one second when
	// refused): the time until the bucket has a token again.
	RetryAfter time.Duration
}

// Deps are the dependencies of the rate limiter.
type Deps struct {
	// Settings are the applied limits of the dynamic configuration
	// (required).
	Settings settings.Service
	// Clock refills the buckets (default the wall clock).
	Clock  clock.Clock
	Logger *slog.Logger
}
