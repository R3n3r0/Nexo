package object

import (
	"time"
)

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
	// storageClasses maps a storage class to its "EC:n" parity setting.
	storageClasses map[string]string
	// poolSpaceTTL bounds the age of the cached free space of the pools
	// (default 5s).
	poolSpaceTTL time.Duration
	// pinnedReadParts is the number of parts a read opens up front to
	// release its read lock before streaming (default 8, 0 disables).
	pinnedReadParts int
	// compression selects the versions stored compressed (nil: none).
	compression CompressionPolicy
}

func defaultOptions() options {
	return options{
		inlineLimit:  128 << 10,
		blockSize:    1 << 20,
		minPartSize:  5 << 20,
		lockTimeout:  30 * time.Second,
		region:       "us-east-1",
		poolSpaceTTL: 5 * time.Second,
		// A read of up to 8 parts opens at most 8 part files (fs) or 8
		// times the data shards (erasure) at once.
		pinnedReadParts: 8,
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

// WithStorageClass sets the parity of a storage class (STANDARD or
// REDUCED_REDUNDANCY) as "EC:n", the format of NEXO_STORAGE_CLASS_STANDARD
// and NEXO_STORAGE_CLASS_RRS. An empty value keeps the default. Invalid
// settings make the erasure constructors fail.
func WithStorageClass(class, value string) Option {
	return func(o *options) {
		if o.storageClasses == nil {
			o.storageClasses = map[string]string{}
		}
		o.storageClasses[class] = value
	}
}

// WithPoolSpaceTTL sets how long the free space of the pools, which
// weights the placement of new objects, is cached.
func WithPoolSpaceTTL(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.poolSpaceTTL = d
		}
	}
}

// WithPinnedReadParts sets the number of parts a GetObject opens before
// streaming them, to release the read lock of the object right away: an
// open file keeps reading the version after a writer replaced or purged
// it (MVCC on the data directory), so a slow or stalled download never
// blocks the writers of its key. Reads spanning more parts hold the lock
// until their body is closed; 0 disables the early release.
func WithPinnedReadParts(n int) Option {
	return func(o *options) {
		if n >= 0 {
			o.pinnedReadParts = n
		}
	}
}

// WithCompression stores compressed the new unencrypted versions policy
// selects (S-OBJ-14).
func WithCompression(policy CompressionPolicy) Option {
	return func(o *options) {
		o.compression = policy
	}
}
