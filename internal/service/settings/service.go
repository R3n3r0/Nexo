// Package settings applies the dynamic configuration (config.Service) to
// the server: it reads the subsystems the server honors at every call, so
// that a change written through the Admin API takes effect on the next
// request without a restart (hot reload), and it decorates the services
// whose behavior depends on them.
//
// Applied keys (Applied reports them, the console and nexoctl show the
// others as "not applied yet"):
//
//   - api.cors_allow_origin: the origins the S3 API allows for the
//     buckets without a CORS configuration of their own;
//   - api.root_access: "off" denies the root credentials on the S3 API
//     (GuardRootAccess); the console, the Admin API and the credentials
//     derived from root (service accounts, STS sessions) keep working, so
//     that an administrator can always turn it back on;
//   - scanner.speed and scanner.cycle: read by the background scanner at
//     the start of every cycle;
//   - api.requests_max, api.requests_deadline and the ratelimit.* keys:
//     the request limits of the node (Limits), read by the rate limiter
//     (internal/service/ratelimit) at every request.
package settings

import (
	"context"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/service/config"
)

// Service exposes the applied settings of the dynamic configuration.
type Service interface {
	// API returns the applied settings of the "api" subsystem, with the
	// defaults of the keys that are not set. A value that cannot be parsed
	// (written around the Admin API validation) keeps the default and is
	// logged.
	API(ctx context.Context) (API, error)
	// Limits returns the applied request limits of the node (S-OPS-10):
	// the concurrency of the S3 API (api.requests_max and
	// api.requests_deadline) and the token buckets of the "ratelimit"
	// subsystem. Unset keys are their defaults (no limit); a value that
	// cannot be parsed keeps the default and is logged.
	Limits(ctx context.Context) (Limits, error)
}

// Rate is a token bucket: PerSecond requests per second on average, up to
// Burst at once. A zero PerSecond is no limit.
type Rate struct {
	PerSecond int
	// Burst is the size of the bucket: PerSecond when the key is not set.
	Burst int
}

// Limits are the request limits of a node. Every limit is per node: on a
// cluster of N nodes behind a load balancer the deployment admits up to N
// times each of them (docs/user/configuration.md).
type Limits struct {
	// RequestsMax is the number of S3 requests a node serves at once
	// (api.requests_max, 0: no limit); RequestsDeadline is how long a
	// request beyond it waits for a slot before the 503 SlowDown
	// (api.requests_deadline, default 10s).
	RequestsMax      int
	RequestsDeadline time.Duration
	// S3IP, S3AccessKey and S3Bucket limit the S3 requests of a client
	// address, of an access key and to a bucket (ratelimit.s3_ip,
	// s3_access_key, s3_bucket and their _burst keys).
	S3IP        Rate
	S3AccessKey Rate
	S3Bucket    Rate
	// AdminIP and AdminUser limit the Admin API requests of a client
	// address and of an authenticated user (ratelimit.admin_ip,
	// admin_user and their _burst keys).
	AdminIP   Rate
	AdminUser Rate
}

// API are the applied settings of the S3 API.
type API struct {
	// RootAccess allows the root credentials on the S3 API
	// (api.root_access, default on).
	RootAccess bool
	// CORSAllowOrigin are the origins of the deployment-wide CORS policy
	// of the S3 API (api.cors_allow_origin, comma separated, "*" for
	// every origin): it applies to the buckets without a CORS
	// configuration. Nil (the key is not set) allows every origin.
	CORSAllowOrigin []string
}

// Deps are the dependencies of the settings service.
type Deps struct {
	// Config is the dynamic configuration (required).
	Config config.Service
	Logger *slog.Logger
}
