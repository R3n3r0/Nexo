package s3

import (
	"net"
	"strings"
)

// Option configures the S3 route groups and the Dispatcher. An option that
// does not concern a constructor is ignored by it.
type Option func(*options)

type options struct {
	// region is the region of the deployment (the S3 default when empty).
	region string
	// domains enable virtual-host style requests.
	domains []string
	// publicMetrics serves the metrics endpoints without authentication.
	publicMetrics bool
}

func newOptions(opts []Option) options {
	var o options
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
