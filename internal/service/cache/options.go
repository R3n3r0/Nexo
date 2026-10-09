package cache

import "time"

// Defaults of the cache service.
const (
	// DefaultMemorySize is the size of the memory tier.
	DefaultMemorySize = 256 << 20
	// DefaultDiskSize is the size of the disk tier (with a directory).
	DefaultDiskSize = 10 << 30
)

const (
	defaultSyncInterval     = 2 * time.Second
	defaultRetryInterval    = time.Second
	defaultAckTimeout       = 2 * time.Second
	defaultMaxPending       = 1024
	defaultWebhookMinDelay  = time.Second
	defaultWebhookMaxDelay  = time.Minute
	defaultHistory          = 100
	defaultLockTimeout      = 10 * time.Second
	defaultWarmBandwidth    = 64 << 20
	defaultWarmConcurrency  = 4
	defaultDiskWriters      = 2
	defaultDiskQueue        = 256
	defaultMemoryEntryRatio = 8
)

// Option configures the cache service.
type Option func(*options)

type options struct {
	mode       Mode
	memorySize int64
	dir        string
	diskSize   int64
	// syncInterval is the period of the comparison of the invalidation
	// sequences with the peers.
	syncInterval time.Duration
	// retryInterval is the period of the retries of the invalidations a
	// peer missed; ackTimeout bounds the wait for a peer in the path of a
	// write; maxPending bounds the invalidations kept for a peer.
	retryInterval time.Duration
	ackTimeout    time.Duration
	maxPending    int
	// webhookDir is the directory of the persistent queue of the purge
	// webhooks; webhookMin and webhookMax bound their retry delay.
	webhookDir string
	webhookMin time.Duration
	webhookMax time.Duration
	// history bounds the invalidations kept per distribution.
	history     int
	lockTimeout time.Duration
}

func defaultOptions() options {
	return options{
		mode: ModeLocal, memorySize: DefaultMemorySize, diskSize: DefaultDiskSize,
		syncInterval: defaultSyncInterval, retryInterval: defaultRetryInterval, ackTimeout: defaultAckTimeout,
		maxPending: defaultMaxPending, webhookMin: defaultWebhookMinDelay, webhookMax: defaultWebhookMaxDelay,
		history: defaultHistory, lockTimeout: defaultLockTimeout,
	}
}

// WithMode sets the cluster mode (default ModeLocal; a single node is
// always local).
func WithMode(m Mode) Option {
	return func(o *options) {
		if m != "" {
			o.mode = m
		}
	}
}

// WithMemorySize sets the size of the memory tier in bytes (default
// 256 MiB).
func WithMemorySize(n int64) Option {
	return func(o *options) {
		if n > 0 {
			o.memorySize = n
		}
	}
}

// WithDir enables the disk tier in dir (--cache-dir): without it only the
// memory tier is used.
func WithDir(dir string) Option {
	return func(o *options) { o.dir = dir }
}

// WithDiskSize sets the size of the disk tier in bytes (--cache-size,
// default 10 GiB).
func WithDiskSize(n int64) Option {
	return func(o *options) {
		if n > 0 {
			o.diskSize = n
		}
	}
}

// WithSyncInterval sets how often the node compares its invalidation
// sequences with its peers (default 2s).
func WithSyncInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.syncInterval = d
		}
	}
}

// WithRetryInterval sets how often the invalidations a peer missed are
// sent again (default 1s).
func WithRetryInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.retryInterval = d
		}
	}
}

// WithAckTimeout bounds the wait for the peers in the path of a write
// (default 2s): a peer slower than that is retried in the background.
func WithAckTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.ackTimeout = d
		}
	}
}

// WithMaxPending bounds the invalidations kept for an unreachable peer
// (default 1024); beyond it they collapse into a flush of its cache.
func WithMaxPending(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxPending = n
		}
	}
}

// WithWebhookQueue sets the directory of the persistent queue of the
// purge webhooks (default <cache-dir>/webhooks; in memory without either).
func WithWebhookQueue(dir string) Option {
	return func(o *options) { o.webhookDir = dir }
}

// WithWebhookRetry bounds the delay between two attempts of a purge
// webhook (default 1s to 1m, doubling).
func WithWebhookRetry(minDelay, maxDelay time.Duration) Option {
	return func(o *options) {
		if minDelay > 0 {
			o.webhookMin = minDelay
		}
		if maxDelay >= o.webhookMin {
			o.webhookMax = maxDelay
		}
	}
}

// WithHistory bounds the invalidations kept per distribution (default
// 100).
func WithHistory(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.history = n
		}
	}
}

// WithLockTimeout bounds the wait for the configuration lock (default
// 10s).
func WithLockTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.lockTimeout = d
		}
	}
}
