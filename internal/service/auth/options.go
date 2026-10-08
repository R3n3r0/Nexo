package auth

import "time"

// Defaults of the verifier and signer options.
const (
	defaultMaxSkew = 15 * time.Minute
	// defaultKeyCacheSize bounds the signing key cache: one entry per
	// (secret, day, region, service).
	defaultKeyCacheSize = 4096
	// defaultMaxUnhashedBody bounds the body the verifier buffers to hash
	// when a signed request carries no X-Amz-Content-Sha256 header
	// (generic SigV4 clients, STS form posts).
	defaultMaxUnhashedBody = 1 << 20
	// defaultRegion is the signing region when none is configured.
	defaultRegion = "us-east-1"
)

// Option configures the SigV4 verifier and signer.
type Option func(*options)

type options struct {
	// region is the region requests must be signed for; empty accepts any.
	region string
	// maxSkew is the tolerated difference between the request time and the
	// server clock (default 15 minutes, as AWS).
	maxSkew time.Duration
	// keyCacheSize bounds the signing key cache; 0 disables it.
	keyCacheSize int
	// maxUnhashedBody bounds the body hashed when X-Amz-Content-Sha256 is
	// missing.
	maxUnhashedBody int64
}

func defaultOptions() options {
	return options{maxSkew: defaultMaxSkew, keyCacheSize: defaultKeyCacheSize, maxUnhashedBody: defaultMaxUnhashedBody}
}

func applyOptions(opts []Option) options {
	o := defaultOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// WithRegion sets the region signatures are checked against (and signed
// with). The empty default accepts any region when verifying and signs for
// us-east-1.
func WithRegion(region string) Option {
	return func(o *options) { o.region = region }
}

// WithMaxClockSkew sets the tolerated request time skew (default 15
// minutes). Non-positive values are ignored.
func WithMaxClockSkew(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.maxSkew = d
		}
	}
}

// WithSigningKeyCacheSize bounds the number of derived signing keys kept in
// memory (default 4096); 0 disables the cache. Negative values are
// ignored.
func WithSigningKeyCacheSize(n int) Option {
	return func(o *options) {
		if n >= 0 {
			o.keyCacheSize = n
		}
	}
}

// WithMaxUnhashedBody bounds the body the verifier reads and hashes when a
// signed request carries no X-Amz-Content-Sha256 header (default 1 MiB);
// larger bodies are rejected. Negative values are ignored.
func WithMaxUnhashedBody(n int64) Option {
	return func(o *options) {
		if n >= 0 {
			o.maxUnhashedBody = n
		}
	}
}
