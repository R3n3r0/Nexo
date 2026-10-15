package s3

import (
	"net"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// Option configures the S3 route groups and the Dispatcher. An option that
// does not concern a constructor is ignored by it.
type Option func(*options)

type options struct {
	// region is the region of the deployment (the S3 default when empty).
	region string
	// domains enable virtual-host style requests.
	domains []string
	// hostBuckets resolves the custom hosts aliasing a bucket.
	hostBuckets httpx.HostBucketResolver
	// publicMetrics serves the metrics endpoints without authentication.
	publicMetrics bool
	// insecureSSEC accepts SSE-C keys over plain HTTP.
	insecureSSEC bool
	// sendTimeout bounds the stalls of an object body (<= 0: unbounded).
	sendTimeout time.Duration
}

func newOptions(opts []Option) options {
	o := options{sendTimeout: httpx.DefaultSendTimeout}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// WithRegion sets the region of the deployment: CreateBucket rejects a
// different location constraint (InvalidLocationConstraint) and
// GetBucketLocation reports buckets without a region in it. Used by
// NewBucketRoutes.
func WithRegion(region string) Option {
	return func(o *options) { o.region = strings.TrimSpace(region) }
}

// WithDomains enables virtual-host style requests ("{bucket}.{domain}") for
// the given domains; ports and surrounding dots are ignored. Used by
// NewDispatcher.
func WithDomains(domains ...string) Option {
	return func(o *options) {
		for _, d := range domains {
			if d = normalizeHost(d); d != "" {
				o.domains = append(o.domains, d)
			}
		}
	}
}

// normalizeHost lowercases a host and strips its port and the surrounding
// dots.
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.Trim(strings.ToLower(host), ".")
}

// WithInsecureSSEC accepts the requests carrying SSE-C keys
// (x-amz-server-side-encryption-customer-*) over plain HTTP; by default
// they need TLS, or a trusted proxy that terminated it, as S3 requires.
// For tests and development only. Used by NewObjectRoutes,
// NewMultipartRoutes, NewPresignedRoutes and NewPostPolicyRoutes.
func WithInsecureSSEC(allow bool) Option {
	return func(o *options) { o.insecureSSEC = allow }
}

// WithSendTimeout bounds how long the body of a GetObject may stall
// (default httpx.DefaultSendTimeout; <= 0 disables the bound): a client
// that stops reading has its response aborted, which releases the read
// lock of the object that the download holds (see httpx.SendGuard). Used
// by NewObjectRoutes.
func WithSendTimeout(d time.Duration) Option {
	return func(o *options) { o.sendTimeout = d }
}

// WithHostBuckets resolves the custom hosts of the buckets before the
// virtual-host domains (the lifecycle and notification middlewares must
// resolve the requests like the API: pass them the same resolver).
func WithHostBuckets(r httpx.HostBucketResolver) Option {
	return func(o *options) { o.hostBuckets = r }
}
