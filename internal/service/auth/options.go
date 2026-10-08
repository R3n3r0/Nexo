package auth

import "time"

// Option configures the SigV4 verifier and signer.
type Option func(*options)

type options struct {
	// region is the region requests must be signed for; empty accepts any.
	region string
	// maxSkew is the tolerated difference between the request time and the
	// server clock (default 15 minutes, as AWS).
	maxSkew time.Duration
}

func defaultOptions() options {
	return options{maxSkew: 15 * time.Minute}
}

// WithRegion sets the region signatures are checked against (and signed
// with). The empty default accepts any region.
func WithRegion(region string) Option {
	return func(o *options) { o.region = region }
}

// WithMaxClockSkew sets the tolerated request time skew.
func WithMaxClockSkew(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.maxSkew = d
		}
	}
}
