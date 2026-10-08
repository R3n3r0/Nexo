package object

import "time"

// Option configures an object layer implementation.
type Option func(*options)

type options struct {
	// inlineLimit is the size up to which object data is stored inline in
	// the metadata file (default 128 KiB).
	inlineLimit int64
	// blockSize is the erasure coding block size (default 1 MiB).
	blockSize int64
	// minPartSize is the minimum size of a non-final multipart part
	// (default 5 MiB, as S3).
	minPartSize int64
	// lockTimeout bounds lock acquisitions (default 30s).
	lockTimeout time.Duration
	// region is the region reported in events and bucket information.
	region string
}

func defaultOptions() options {
	return options{
		inlineLimit: 128 << 10,
		blockSize:   1 << 20,
		minPartSize: 5 << 20,
		lockTimeout: 30 * time.Second,
		region:      "us-east-1",
	}
}

// WithInlineLimit sets the size up to which objects are stored inline in
// their metadata (0 disables inlining).
func WithInlineLimit(n int64) Option {
	return func(o *options) {
		if n >= 0 {
			o.inlineLimit = n
		}
	}
}

// WithBlockSize sets the erasure coding block size.
func WithBlockSize(n int64) Option {
	return func(o *options) {
		if n > 0 {
			o.blockSize = n
		}
	}
}

// WithMinPartSize sets the minimum size of non-final multipart parts.
func WithMinPartSize(n int64) Option {
	return func(o *options) {
		if n >= 0 {
			o.minPartSize = n
		}
	}
}

// WithLockTimeout bounds lock acquisitions.
func WithLockTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.lockTimeout = d
		}
	}
}

// WithRegion sets the region reported in events and bucket information.
func WithRegion(region string) Option {
	return func(o *options) {
		if region != "" {
			o.region = region
		}
	}
}
