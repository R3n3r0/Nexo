package bucket

import "time"

// Defaults of the bucket service options.
const (
	defaultRegion          = "us-east-1"
	defaultMaxDocumentSize = 1 << 20
	defaultLockTimeout     = 30 * time.Second
)

// Option configures the bucket service.
type Option func(*options)

type options struct {
	// region is the default region of new buckets.
	region string
	// maxDocumentSize bounds the stored XML/JSON documents.
	maxDocumentSize int
	// lockTimeout bounds the acquisition of a bucket's configuration lock.
	lockTimeout time.Duration
}

func defaultOptions() options {
	return options{region: defaultRegion, maxDocumentSize: defaultMaxDocumentSize, lockTimeout: defaultLockTimeout}
}

// newOptions applies opts (nil entries are ignored) over the defaults.
func newOptions(opts []Option) options {
	o := defaultOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// WithRegion sets the default region of new buckets (default us-east-1).
func WithRegion(region string) Option {
	return func(o *options) {
		if region != "" {
			o.region = region
		}
	}
}

// WithMaxDocumentSize bounds policy and XML configuration documents
// (default 1 MiB).
func WithMaxDocumentSize(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxDocumentSize = n
		}
	}
}

// WithLockTimeout bounds the acquisition of a bucket's configuration lock
// (default 30s).
func WithLockTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.lockTimeout = d
		}
	}
}
