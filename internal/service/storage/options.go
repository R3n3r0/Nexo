package storage

import "time"

// Option configures the local storage implementation.
type Option func(*options)

type options struct {
	// fsync controls whether data and metadata writes are synced to disk
	// before being committed (default true).
	fsync bool
	// cleanTmp empties the system tmp directory at startup (default true).
	cleanTmp bool
	// dirPerm and filePerm are the permissions of created entries.
	dirPerm  uint32
	filePerm uint32
}

func defaultOptions() options {
	return options{fsync: true, cleanTmp: true, dirPerm: 0o750, filePerm: 0o640}
}

// WithoutFsync skips fsync calls; data durability is then left to the
// operating system (tests, benchmarks).
func WithoutFsync() Option {
	return func(o *options) { o.fsync = false }
}

// WithoutTmpCleanup keeps the content of the system tmp directory at startup.
func WithoutTmpCleanup() Option {
	return func(o *options) { o.cleanTmp = false }
}

// JanitorOption configures the janitor.
type JanitorOption func(*janitorOptions)

type janitorOptions struct {
	// interval is the time between two sweeps (default 1h, 0 disables
	// the background sweeps).
	interval time.Duration
	// maxAge is the age past which an entry is stale (default 24h).
	maxAge time.Duration
}

func defaultJanitorOptions() janitorOptions {
	return janitorOptions{interval: time.Hour, maxAge: 24 * time.Hour}
}

// WithSweepInterval sets the time between two background sweeps (0
// disables them: Sweep runs on demand only).
func WithSweepInterval(d time.Duration) JanitorOption {
	return func(o *janitorOptions) {
		if d >= 0 {
			o.interval = d
		}
	}
}

// WithMaxTmpAge sets the age past which a temporary entry is stale.
func WithMaxTmpAge(d time.Duration) JanitorOption {
	return func(o *janitorOptions) {
		if d > 0 {
			o.maxAge = d
		}
	}
}
